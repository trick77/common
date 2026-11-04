// Package webfetch is a dependency-light Go port of the reference Python
// "mcp-server-fetch" tool (github.com/modelcontextprotocol/servers, src/fetch).
// It fetches a URL, optionally extracts the page's main content as Markdown, and
// returns text ready to hand to an LLM.
//
// The observable contract of the upstream tool is reproduced closely: the
// autonomous User-Agent string, the HTML/raw content-type heuristic, the
// "Contents of <url>:" wrapper, and the truncation / error strings.
//
// The one unavoidable deviation is content extraction: upstream runs Mozilla
// Readability.js in a Node subprocess (readabilipy use_readability=True) plus
// Python markdownify. That JS pipeline cannot be reproduced byte-for-byte in
// pure Go, so we use a trimmed copy of codeberg.org/readeck/go-readability (a
// maintained Go port of the same Readability.js, in internal/readability)
// followed by JohannesKaufmann/html-to-markdown
// configured to match markdownify's defaults (ATX headings, "*" bullets, "*"
// emphasis). On typical pages this is byte-identical to the Python output; the
// only observed difference is readability's URL normalization (e.g. a trailing
// slash added to bare links). Staying in-process (no Node, no subprocess) is
// also what makes the sidecar container removable, which is the point of this
// package.
//
// Beyond upstream, Options offers opt-in extensions (IncludeMetadata,
// PDFHandler, FullPage / Selector / ExcludeSelectors) that default to off, and
// one deliberate default: MaxBodyBytes caps response bodies at 10 MiB so a
// model-chosen URL cannot exhaust memory. Only bodies over the cap behave
// differently from upstream (they fail instead of being read whole).
package webfetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/trick77/toolkit/webfetch/internal/readability"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/unicode/norm"
)

// DefaultUserAgentAutonomous is the User-Agent sent for autonomous (tool-driven)
// fetches. It is copied verbatim from upstream mcp-server-fetch; the reference
// server presents this generic identity rather than the real client, and we
// preserve that behaviour intentionally.
const DefaultUserAgentAutonomous = "ModelContextProtocol/1.0 (Autonomous; +https://github.com/modelcontextprotocol/servers)"

// defaultMaxLength mirrors the upstream Fetch.max_length default.
const defaultMaxLength = 5000

// DefaultMaxBodyBytes is the response-body cap applied when Options.MaxBodyBytes
// is zero. Upstream reads bodies unbounded; the cap is a deliberate divergence
// that only affects bodies larger than this.
const DefaultMaxBodyBytes = 10 << 20

// DefaultMaxPDFBytes is the body cap for PDF responses handed to
// Options.PDFHandler when Options.MaxPDFBytes is zero.
const DefaultMaxPDFBytes = 50 << 20

// fetchTimeout mirrors upstream's httpx timeout=30.
const fetchTimeout = 30 * time.Second

// maxRedirects mirrors httpx's default max_redirects (Go's default is 10).
const maxRedirects = 20

// Content sentinels. The first two are upstream's, reproduced verbatim.
const (
	errNoMoreContent = "<error>No more content available.</error>"
	errSimplify      = "<error>Page failed to be simplified from HTML</error>"
	errNoSelector    = "<error>No content matched the selector.</error>" // own, for Selector
)

// Own sentinels for a Selector / ExcludeSelectors entry the CSS parser
// rejects (bad syntax, or valid CSS it doesn't support): the
// (normalized) selector, as is (Go quoting would double CSS backslashes), and
// the CSS parser's message.
const (
	errBadSelector        = `<error>Unsupported or invalid selector "%s": %s.</error>`
	errBadExcludeSelector = `<error>Unsupported or invalid exclude selector "%s": %s.</error>`
)

// Options mirror the upstream tool's parameters.
type Options struct {
	// MaxLength is the maximum number of characters to return. Zero means the
	// upstream default (5000).
	MaxLength int
	// StartIndex returns output starting at this character index, for paging a
	// previously truncated fetch.
	StartIndex int
	// Raw returns the actual HTML without Markdown simplification.
	Raw bool
	// UserAgent overrides the autonomous User-Agent. Empty uses the default.
	UserAgent string
	// MaxBodyBytes caps the size of the response body. A body larger than the
	// cap is rejected with a "Failed to fetch" error before any decoding, so a
	// model-chosen URL cannot exhaust memory. Zero (the default) applies
	// DefaultMaxBodyBytes (10 MiB); a negative value disables the cap, which is
	// upstream's unbounded behaviour. Bodies under the cap are unaffected.
	MaxBodyBytes int64
	// IncludeMetadata, when true, prepends a small YAML frontmatter block
	// (title, author, published, site, language — non-empty fields only) ahead
	// of the extracted Markdown. It applies only to the HTML-simplification path
	// (not Raw and not non-HTML content). Default false, which keeps the output
	// byte-identical to upstream mcp-server-fetch.
	//
	// The frontmatter counts as part of the returned content, so StartIndex /
	// MaxLength page over it too; hold IncludeMetadata constant across paged
	// calls so a page-2 StartIndex stays aligned.
	IncludeMetadata bool
	// PDFHandler, when set, receives the body of a PDF response (detected by
	// the "%PDF-" magic bytes, whatever the Content-Type) and returns its text,
	// which is then returned like any other content instead of the raw bytes
	// behind the "cannot be simplified" note. webfetch does not parse PDFs
	// itself: the bytes are untrusted, so run the parser somewhere a malformed
	// or malicious PDF cannot take the caller down (e.g. a Tika sidecar). The
	// handler must bound its own run time; webfetch's fetch timeout covers only
	// the download. A handler error or panic, or text that is empty after
	// trimming (e.g. a scan without OCR), fails the fetch with a "Failed to
	// extract PDF" error. Raw takes precedence. Nil (the default) keeps the
	// upstream raw-bytes behaviour.
	PDFHandler func(ctx context.Context, body []byte) (string, error)
	// MaxPDFBytes caps PDF bodies in place of MaxBodyBytes while PDFHandler is
	// set; the two caps are independent. Zero applies DefaultMaxPDFBytes
	// (50 MiB); a negative value disables the cap.
	MaxPDFBytes int64
	// FullPage converts the entire page to Markdown, skipping the Readability
	// main-content extraction. Use it when Readability over-strips (docs pages,
	// tables, sidebars you actually want). Ignored when Selector is set, and when
	// Raw is set. IncludeMetadata is not applied on this path. The text is
	// normalized as on the Readability path (NFC, soft hyphens removed), and
	// Selector / ExcludeSelectors are normalized to match it; text the page
	// writes as a reference (&shy;, &#x301;) stays as written, so a selector
	// with NFD / soft-hyphen text does not match it. Default false.
	FullPage bool
	// Selector, when set, converts only the element(s) matching this CSS selector
	// to Markdown, skipping Readability (an escape hatch for targeting a specific
	// region). Takes precedence over FullPage. Ignored when Raw is set.
	// IncludeMetadata is not applied on this path; text is normalized as with
	// FullPage. If nothing matches, the content is the
	// "<error>No content matched the selector.</error>" sentinel (with a nil
	// error). A blank Selector (whitespace only once normalized) counts as
	// unset; others go to the CSS parser as given. On an HTML page, one the
	// parser rejects (any member of a group) yields
	// "<error>Unsupported or invalid selector "<sel>": <reason>.</error>"
	// instead, also with a nil error; <sel> and <reason> are shown with
	// control characters as spaces, < > as ‹ ›, and cut to 200 runes.
	// Default "".
	Selector string
	// ExcludeSelectors removes element(s) matching these CSS selectors before
	// conversion. Unlike FullPage/Selector it composes with every non-raw mode,
	// including the default Readability path (e.g. strip a cookie banner, then
	// simplify). Empty (the default) leaves the input untouched, so output stays
	// byte-identical to upstream. Ignored when Raw is set. Blank entries are
	// dropped (as Selector), others used as given. On an HTML page, an entry
	// the parser rejects fails the extraction rather than keep what it should
	// drop: the content is only
	// "<error>Unsupported or invalid exclude selector "<sel>": <reason>.</error>"
	// (with a nil error), shown as for Selector. The selectors are
	// normalized (NFC, soft hyphens removed) on every mode, the Readability path
	// included. A backslash-escaped soft hyphen is not normalized like a literal
	// one: \AD matches only text the page writes as &shy;, and \ before a
	// literal one escapes the next character instead.
	ExcludeSelectors []string
}

// Fetch fetches the URL, extracts/keeps the content, applies
// start_index/max_length paging, and returns the text wrapped as
// "<prefix>Contents of <url>:\n<content>". Outbound connections are restricted
// to public IPs by the SSRF guard in the dialer.
//
// It returns a non-nil error on connection failure, HTTP status >= 400, a
// response body over MaxBodyBytes (or MaxPDFBytes), or (with PDFHandler) a PDF
// the handler fails on or that yields no text. Callers that have an alternate reader (e.g.
// a headless-browser fallback) should treat a non-nil error as "try the
// fallback".
func Fetch(ctx context.Context, rawURL string, opts Options) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", fmt.Errorf("URL is required")
	}
	opts = opts.withDefaults()

	content, prefix, err := fetchURL(ctx, rawURL, opts)
	if err != nil {
		return "", err
	}

	// Character (code-point) indexing, matching Python str slicing. Upstream
	// checks "start_index >= len(content)" and then "not truncated_content";
	// with max_length > 0 the second is implied by the first, so one empty
	// check covers both.
	out, got, total := sliceRunes(content, opts.StartIndex, opts.MaxLength)
	if out == "" {
		out = errNoMoreContent
	} else if remaining := total - (opts.StartIndex + got); got == opts.MaxLength && remaining > 0 {
		out += fmt.Sprintf("\n\n<error>Content truncated. Call the fetch tool with a start_index of %d to get more content.</error>", opts.StartIndex+got)
	}
	return fmt.Sprintf("%sContents of %s:\n%s", prefix, rawURL, out), nil
}

// withDefaults resolves the zero-value defaults: the autonomous User-Agent,
// the upstream MaxLength, a non-negative StartIndex, DefaultMaxBodyBytes and
// DefaultMaxPDFBytes. A negative cap stays negative and means unlimited.
func (o Options) withDefaults() Options {
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgentAutonomous
	}
	if o.MaxLength <= 0 {
		o.MaxLength = defaultMaxLength
	}
	if o.StartIndex < 0 {
		o.StartIndex = 0
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if o.MaxPDFBytes == 0 {
		o.MaxPDFBytes = DefaultMaxPDFBytes
	}
	return o
}

// fetchURL fetches the URL with already-resolved opts. content is either
// extracted Markdown or the raw body, always valid UTF-8; prefix is the
// non-empty note prepended for non-simplifiable content types, matching
// upstream.
func fetchURL(ctx context.Context, rawURL string, opts Options) (content, prefix string, err error) {
	client := &http.Client{
		Timeout:       fetchTimeout,
		Transport:     sharedTransport(),
		CheckRedirect: checkRedirect,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fetchErr(rawURL, err)
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fetchErr(rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("Failed to fetch %s - status code %d", rawURL, resp.StatusCode) //nolint:staticcheck // ST1005: upstream contract
	}

	// A PDF is recognised by its magic bytes alone, never by the Content-Type
	// header, so a mislabelled body cannot claim the larger PDF cap. Peeking
	// lets the cap be chosen before the body is read.
	var body io.Reader = resp.Body
	isPDF := false
	if opts.PDFHandler != nil && !opts.Raw {
		br := bufio.NewReader(resp.Body)
		magic, _ := br.Peek(len(pdfMagic))
		isPDF = bytes.Equal(magic, pdfMagic)
		body = br
	}
	limit := opts.MaxBodyBytes
	if isPDF {
		limit = opts.MaxPDFBytes
	}
	if limit > 0 && resp.ContentLength > limit {
		return "", "", fetchErr(rawURL, errBodyTooLarge(limit))
	}
	bodyBytes, err := readBody(body, limit)
	if err != nil {
		return "", "", fetchErr(rawURL, err)
	}
	contentType := resp.Header.Get("content-type")

	// PDF handling runs on the raw bytes, before charset decoding (which would
	// corrupt binary content). Raw takes precedence, matching the option's doc.
	if isPDF {
		text, pdfErr := callPDFHandler(ctx, opts.PDFHandler, bodyBytes)
		if pdfErr == nil && strings.TrimSpace(text) == "" {
			pdfErr = errors.New("no extractable text")
		}
		if pdfErr != nil {
			return "", "", fmt.Errorf("Failed to extract PDF %s: %w", rawURL, pdfErr) //nolint:staticcheck // ST1005: matches the upstream-style messages
		}
		return strings.ToValidUTF8(strings.TrimSpace(text), "�"), "", nil
	}

	pageRaw, err := decodeBody(bodyBytes, contentType)
	if err != nil {
		return "", "", fetchErr(rawURL, err)
	}

	// Upstream: '"<html" in page_raw[:100]' — a character slice, not bytes.
	isPageHTML := strings.Contains(firstRunes(pageRaw, 100), "<html") ||
		strings.Contains(contentType, "text/html") ||
		contentType == ""

	if isPageHTML && !opts.Raw {
		// Links resolve against the final URL (after redirects); the wrapper
		// keeps the requested URL, as upstream does.
		return extractContentFromHTML(pageRaw, resp.Request.URL, opts), "", nil
	}
	return pageRaw, fmt.Sprintf("Content type %s cannot be simplified to markdown, but here is the raw content:\n", contentType), nil
}

// checkRedirect follows up to maxRedirects hops, matching httpx's default
// rather than net/http's 10. via holds every request sent so far (the initial
// one plus each followed hop), so hop N sees len(via) == N.
func checkRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// fetchErr wraps err as upstream's "Failed to fetch <url>: <err>". The
// capitalized prefix is upstream's, reproduced verbatim as part of this
// package's observable contract (err itself may be this package's own, e.g.
// the body cap); ST1005 is suppressed per site rather than in the lint
// config, which ship embeds identically across the repo family.
func fetchErr(rawURL string, err error) error {
	return fmt.Errorf("Failed to fetch %s: %w", rawURL, err) //nolint:staticcheck // ST1005: upstream contract
}

func errBodyTooLarge(limit int64) error {
	return fmt.Errorf("response body exceeds %d bytes", limit)
}

// readBody reads r in full. With limit > 0 a body longer than limit bytes is
// rejected (after reading at most limit+1 bytes) rather than buffered whole;
// limit <= 0 means unlimited. math.MaxInt64 is treated as unlimited so
// limit+1 cannot overflow.
func readBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 || limit == math.MaxInt64 {
		return io.ReadAll(r)
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errBodyTooLarge(limit)
	}
	return b, nil
}

// decodeBody converts the body to a valid UTF-8 string. The charset is taken,
// in order, from the Content-Type header, a BOM, or an HTML meta charset (the
// browser rules charset.DetermineEncoding implements); with none of those a
// valid-UTF-8 body is taken as UTF-8, and only a body that is not valid UTF-8
// falls back to the library's windows-1252 guess.
//
// This is a superset of upstream's httpx response.text, which honours the
// header charset and otherwise assumes UTF-8: the BOM / meta / legacy-guess
// steps only kick in on pages upstream would have garbled. The UTF-8 check is
// what keeps the library's last-resort guess from turning a UTF-8 body into
// mojibake whenever its first KiB happens to be pure ASCII. Valid UTF-8 also
// skips the decoder copy entirely.
func decodeBody(body []byte, contentType string) (string, error) {
	enc, name, certain := charset.DetermineEncoding(body, contentType)
	if utf8.Valid(body) && (name == "utf-8" || !certain) {
		return string(body), nil
	}
	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return "", err
	}
	// Decoders substitute U+FFFD for undecodable input; enforce the invariant
	// regardless, since sliceRunes relies on it.
	return strings.ToValidUTF8(string(decoded), "\uFFFD"), nil
}

// firstRunes returns the first n runes of s (all of s if shorter).
func firstRunes(s string, n int) string {
	for off := range s {
		if n == 0 {
			return s[:off]
		}
		n--
	}
	return s
}

// sliceRunes returns s[start:start+n] in rune (code-point) terms, the number of
// runes in that slice, and the total rune count of s. For valid UTF-8 (which
// fetchURL guarantees) this equals string([]rune(s)[start:start+n]) without
// materialising a 4-byte-per-rune copy of the whole content. Requires n > 0.
func sliceRunes(s string, start, n int) (sub string, got, total int) {
	from, to := -1, -1
	for off := range s {
		switch total {
		case start:
			from = off
		case start + n:
			to = off
		}
		total++
	}
	if from < 0 {
		return "", 0, total
	}
	if to < 0 {
		return s[from:], total - start, total
	}
	return s[from:to], n, total
}

// extractContentFromHTML extracts the main article content and converts it to
// Markdown, mirroring upstream's readabilipy + markdownify(ATX). On extraction
// failure it returns the same error sentinel upstream returns. base (may be
// nil) is the final page URL, used to absolutize links.
func extractContentFromHTML(page string, base *url.URL, opts Options) string {
	var (
		article readability.Article
		err     error
	)
	// Blank selectors must not leave the default path.
	opts = opts.cleanSelectors()
	// Escape-hatch pre-pass. ExcludeSelectors composes with every mode (including
	// the default Readability path); Selector / FullPage skip Readability. When
	// none are set this block is skipped entirely and the output is byte-identical
	// to upstream.
	if len(opts.ExcludeSelectors) > 0 || opts.Selector != "" || opts.FullPage {
		cs, bad := compileSelectors(opts)
		if bad != "" {
			return bad
		}
		skipReadability := cs.sel != nil || opts.FullPage
		// Readability's Parse normalizes the text (NFC, soft hyphens removed)
		// before parsing; every opt-in mode does the same, so a page reads the
		// same whichever mode extracts it.
		var src io.Reader = strings.NewReader(page)
		if !isNormalText(page) {
			src = readability.NormalizeText(src)
		}
		doc, perr := goquery.NewDocumentFromReader(src)
		if perr != nil {
			return errSimplify
		}
		for _, m := range cs.exclude {
			doc.FindMatcher(m).Remove()
		}
		if skipReadability {
			// Readability absolutizes links and drops javascript: links
			// itself; do the same here so both paths agree (the latter in
			// selectorMarkdown, after matching).
			absolutizeLinks(doc, base)
			return selectorMarkdown(doc, cs.sel)
		}
		// ExcludeSelectors only: hand the pruned tree straight to Readability.
		// ParseAndMutate needs no copy: the tree is not used again.
		parser := readability.NewParser()
		article, err = parser.ParseAndMutate(doc.Nodes[0], base)
	} else {
		article, err = readability.FromReader(strings.NewReader(page), base)
	}
	if err != nil || article.Node == nil {
		return errSimplify
	}
	var cleaned strings.Builder
	if renderErr := article.RenderHTML(&cleaned); renderErr != nil || strings.TrimSpace(cleaned.String()) == "" {
		return errSimplify
	}
	markdown, err := convertHTMLToMarkdown(cleaned.String())
	if err != nil || strings.TrimSpace(markdown) == "" {
		return errSimplify
	}
	if opts.IncludeMetadata {
		if fm := articleFrontmatter(article); fm != "" {
			return fm + markdown
		}
	}
	return markdown
}

// cleanSelectors normalizes Selector and ExcludeSelectors like the tree
// (normalizeText), unsets a blank Selector and drops blank ExcludeSelectors.
// Blank is empty or whitespace only (unicode.IsSpace) after normalizing: no
// such selector can match. Others are kept as is: cascadia skips CSS
// whitespace padding itself, and trimming would break an escaped trailing
// space ("#a\ ") or a no-break space, which is part of a CSS name.
func (o Options) cleanSelectors() Options {
	o.Selector = cleanSelector(o.Selector)
	var excl []string
	for _, sel := range o.ExcludeSelectors {
		if sel = cleanSelector(sel); sel != "" {
			excl = append(excl, sel)
		}
	}
	o.ExcludeSelectors = excl
	return o
}

func cleanSelector(sel string) string {
	if sel = normalizeText(sel); strings.TrimSpace(sel) == "" {
		return ""
	}
	return sel
}

// compiledSelectors holds the cleaned Selector (nil when unset) and
// ExcludeSelectors, each compiled once for every match pass.
type compiledSelectors struct {
	sel     goquery.Matcher
	exclude []goquery.Matcher
}

// compileSelectors compiles the cleaned selectors of o with cascadia.Compile,
// the parser goquery's Find uses, so a valid selector matches exactly as
// before. A selector it rejects, which Find would silently treat as matching
// nothing, is returned as bad: the sentinel naming it. ExcludeSelectors are
// checked first, as they are applied first.
func compileSelectors(o Options) (cs compiledSelectors, bad string) {
	for _, s := range o.ExcludeSelectors {
		m, err := cascadia.Compile(s)
		if err != nil {
			return compiledSelectors{}, fmt.Sprintf(errBadExcludeSelector, selectorShown(s), selectorShown(selectorReason(s, err)))
		}
		cs.exclude = append(cs.exclude, m)
	}
	if o.Selector != "" {
		m, err := cascadia.Compile(o.Selector)
		if err != nil {
			return compiledSelectors{}, fmt.Sprintf(errBadSelector, selectorShown(o.Selector), selectorShown(selectorReason(o.Selector, err)))
		}
		cs.sel = m
	}
	return cs, ""
}

// selectorReason is cascadia's message for sel, minus the Go-quoted copy of
// sel it prefixes to "N bytes left over": the sentinel shows sel already.
func selectorReason(sel string, err error) string {
	return strings.TrimPrefix(err.Error(), fmt.Sprintf("parsing %q: ", sel))
}

// maxSelectorShown caps the runes of a selector, or of a reason, in a sentinel.
const maxSelectorShown = 200

// selectorShown is s made safe to show inside an <error> sentinel, still
// readable: a control character (newline, NUL, …) becomes a space and < > become
// ‹ ›, so the caller's input can't break the wrapper, and past
// maxSelectorShown runes it is cut with "…". Backslashes stay as written.
func selectorShown(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxSelectorShown {
			b.WriteRune('…')
			break
		}
		switch {
		case unicode.IsControl(r):
			r = ' '
		case r == '<':
			r = '‹'
		case r == '>':
			r = '›'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// isNormalText reports whether readability.NormalizeText would leave s as
// is: it is valid UTF-8 (the transform turns invalid bytes into U+FFFD), NFC and
// has no soft hyphen (almost every page).
func isNormalText(s string) bool {
	return utf8.ValidString(s) && norm.NFC.IsNormalString(s) && !strings.ContainsRune(s, 0xAD)
}

// normalizeText applies readability.NormalizeText to s, without the copy when
// isNormalText.
func normalizeText(s string) string {
	if isNormalText(s) {
		return s
	}
	b, err := io.ReadAll(readability.NormalizeText(strings.NewReader(s)))
	if err != nil {
		return s
	}
	return string(b)
}

// selectorMarkdown converts the subtree(s) matching m (Selector) or, with a nil
// m, the whole body (FullPage) of an already-normalized, pruned,
// link-absolutized document to Markdown, skipping Readability.
func selectorMarkdown(doc *goquery.Document, m goquery.Matcher) string {
	var fragment string
	if m != nil {
		sel := doc.FindMatcher(m)
		// A match nested inside another match is already part of the outer
		// one's HTML; keep only the outermost so nothing is emitted twice.
		sel = sel.NotSelection(sel.FindMatcher(m))
		if sel.Length() == 0 {
			return errNoSelector
		}
		dropScriptLinks(sel)
		var b strings.Builder
		sel.Each(func(_ int, s *goquery.Selection) {
			if h, err := goquery.OuterHtml(s); err == nil {
				b.WriteString(h)
			}
		})
		fragment = b.String()
	} else { // FullPage
		dropScriptLinks(doc.Selection)
		if body := doc.Find("body"); body.Length() > 0 {
			fragment, _ = body.Html()
		} else {
			fragment, _ = doc.Html()
		}
	}
	markdown, err := convertHTMLToMarkdown(fragment)
	if err != nil || strings.TrimSpace(markdown) == "" {
		return errSimplify
	}
	return markdown
}

// dropScriptLinks strips the href of every javascript: link in sel (the
// selected elements and their descendants) so the converter renders only the
// link text, as Readability does. The scheme test is case-insensitive and
// trims whitespace, so it is stricter than Readability's, which keeps
// "JavaScript:" or " javascript:" links. The element itself stays, so it runs
// after Selector matching without changing what matched.
func dropScriptLinks(sel *goquery.Selection) {
	sel.Find("a[href]").AddSelection(sel.Filter("a[href]")).Each(func(_ int, s *goquery.Selection) {
		if href, _ := s.Attr("href"); strings.HasPrefix(strings.ToLower(strings.TrimSpace(href)), "javascript:") {
			s.RemoveAttr("href")
		}
	})
}

// absolutizeLinks resolves every relative href/src in doc against base, in
// place, following Readability's rules for the attributes the Markdown
// converter reads: absolute references (including data:, mailto: and
// javascript: URIs; the last are dropped later by dropScriptLinks),
// fragment-only references ("#top"), empty values and unparsable values are
// left untouched. A nil base is a no-op.
func absolutizeLinks(doc *goquery.Document, base *url.URL) {
	if base == nil {
		return
	}
	doc.Find("[href], [src]").Each(func(_ int, s *goquery.Selection) {
		for _, attr := range [...]string{"href", "src"} {
			raw, ok := s.Attr(attr)
			if !ok {
				continue
			}
			raw = strings.TrimSpace(raw)
			if raw == "" || strings.HasPrefix(raw, "#") {
				continue
			}
			ref, err := url.Parse(raw)
			if err != nil || ref.IsAbs() {
				continue
			}
			s.SetAttr(attr, base.ResolveReference(ref).String())
		}
	})
}

// convertHTMLToMarkdown converts an HTML fragment with the markdownify-matching
// options (ATX headings, "*" bullets, "*" emphasis). Links are passed through
// untouched (empty domain): both callers absolutize them beforehand, and the
// converter's own domain handling would force an "http" scheme.
func convertHTMLToMarkdown(fragment string) (string, error) {
	converter := md.NewConverter("", true, &md.Options{
		HeadingStyle:     "atx",
		BulletListMarker: "*",
		EmDelimiter:      "*",
	})
	return converter.ConvertString(fragment)
}

// articleFrontmatter builds a small YAML frontmatter block from the metadata
// Readability already parsed (title, byline, published time, site name,
// language). Empty fields are omitted; if nothing is populated it returns "".
// Values are emitted as double-quoted YAML scalars so titles/bylines containing
// ":", "#", quotes, or a leading "-" cannot produce malformed frontmatter.
func articleFrontmatter(a readability.Article) string {
	var b strings.Builder
	add := func(key, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		fmt.Fprintf(&b, "%s: %s\n", key, yamlQuote(val))
	}
	add("title", a.Title())
	add("author", a.Byline())
	if pt, err := a.PublishedTime(); err == nil && !pt.IsZero() {
		add("published", pt.Format(time.RFC3339))
	}
	add("site", a.SiteName())
	add("language", a.Language())
	if b.Len() == 0 {
		return ""
	}
	return "---\n" + b.String() + "---\n\n"
}

// yamlQuote renders s as a double-quoted YAML scalar, escaping backslashes and
// double quotes and flattening any embedded newlines/tabs to spaces so the
// value stays on a single frontmatter line.
func yamlQuote(s string) string {
	s = strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", " ",
		"\r", " ",
		"\t", " ",
	).Replace(s)
	return `"` + s + `"`
}

// pdfMagic starts every PDF, whatever Content-Type it is served with.
var pdfMagic = []byte("%PDF-")

// callPDFHandler runs the caller's handler, turning a panic into an error so a
// malformed PDF fails the fetch instead of the caller's process.
func callPDFHandler(ctx context.Context, h func(context.Context, []byte) (string, error), body []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf handler panicked: %v", r)
		}
	}()
	return h(ctx, body)
}

var (
	transportOnce sync.Once
	transport     *http.Transport
)

// sharedTransport returns the package-wide HTTP transport, built once. Sharing
// it across calls reuses connections and TLS sessions instead of stranding an
// idle keep-alive connection (and its goroutines) per fetch. Its dialer
// enforces the SSRF guard on every new connection; a pooled connection was
// validated when it was dialed. Redirects are followed by the client (like
// httpx follow_redirects=True) and each hop is re-dialed through the guard.
func sharedTransport() *http.Transport {
	transportOnce.Do(func() {
		transport = &http.Transport{
			// No proxy, deliberately: a proxy would move egress outside the
			// guarded dialer, which is where the SSRF check lives.
			Proxy:                 nil,
			DialContext:           dialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	})
	return transport
}
