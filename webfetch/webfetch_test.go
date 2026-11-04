package webfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/trick77/toolkit/webfetch/internal/readability"
	"golang.org/x/text/unicode/norm"
)

// allowLoopback relaxes the SSRF guard so tests can reach the loopback
// httptest server, then restores it. Production keeps the strict guard.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := dialControl
	dialControl = nil
	t.Cleanup(func() {
		dialControl = prev
		// Drop pooled connections opened while the guard was relaxed so a
		// guard-on test can never ride one to loopback.
		sharedTransport().CloseIdleConnections()
	})
}

func TestFetch_HTMLToMarkdown(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>T</title></head><body>
			<article><h1>Hello World</h1><p>This is the main content of the article that readability should keep because it is long enough to be considered the primary body text of the page.</p></article>
			</body></html>`)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/page", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.HasPrefix(out, fmt.Sprintf("Contents of %s/page:\n", srv.URL)) {
		t.Fatalf("missing wrapper prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "# Hello World") {
		t.Fatalf("expected ATX heading in markdown, got:\n%s", out)
	}
	if !strings.Contains(out, "main content of the article") {
		t.Fatalf("expected body text, got:\n%s", out)
	}
}

func TestFetch_IncludeMetadata(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="en"><head>
			<title>Great: Article #1 "quoted"</title>
			<meta name="author" content="Jane Doe">
			<meta property="article:published_time" content="2024-01-02T15:04:05Z">
			<meta property="og:site_name" content="Example Blog">
			</head><body>
			<article><h1>Great Article</h1><p>This is the main content of the article that readability should keep because it is long enough to be considered the primary body text of the page.</p></article>
			</body></html>`)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/post", Options{IncludeMetadata: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	// Frontmatter sits inside the wrapper, ahead of the markdown.
	fmStart := fmt.Sprintf("Contents of %s/post:\n---\n", srv.URL)
	if !strings.HasPrefix(out, fmStart) {
		t.Fatalf("expected frontmatter after wrapper, got:\n%s", out)
	}
	for _, want := range []string{
		`title: "Great: Article #1 \"quoted\""`, // ":", "#" and quotes escaped/safe
		`author: "Jane Doe"`,
		`published: "2024-01-02T15:04:05Z"`,
		`site: "Example Blog"`,
		`language: "en"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected frontmatter line %q, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\n---\n\nThis is the main content") {
		t.Fatalf("expected frontmatter to close before the markdown body, got:\n%s", out)
	}

	// Default (no IncludeMetadata) must not emit any frontmatter — fidelity.
	plain, err := Fetch(context.Background(), srv.URL+"/post", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(plain, "---\n") || strings.Contains(plain, "title:") {
		t.Fatalf("default output must not contain frontmatter, got:\n%s", plain)
	}
}

func TestFetch_PDFHandler(t *testing.T) {
	allowLoopback(t)
	pdfBytes := []byte("%PDF-1.4\nopaque body")
	srv := serve(t, "application/pdf", pdfBytes)
	var got []byte
	handler := func(_ context.Context, body []byte) (string, error) {
		got = body
		return "EXTRACTED-TEXT", nil
	}

	// With a handler, the PDF bytes go to it and its text is the content.
	out, err := Fetch(context.Background(), srv.URL+"/doc.pdf", Options{PDFHandler: handler})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if want := fmt.Sprintf("Contents of %s/doc.pdf:\nEXTRACTED-TEXT", srv.URL); out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if !bytes.Equal(got, pdfBytes) {
		t.Fatalf("handler got %d bytes, want the %d-byte body", len(got), len(pdfBytes))
	}

	// Raw takes precedence: the handler is not called.
	got = nil
	raw, err := Fetch(context.Background(), srv.URL+"/doc.pdf", Options{Raw: true, PDFHandler: handler})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if got != nil || !strings.Contains(raw, "cannot be simplified") {
		t.Fatalf("Raw must bypass the handler, got:\n%.200s", raw)
	}

	// Default (no handler) -> upstream behaviour: raw bytes behind the note.
	plain, err := Fetch(context.Background(), srv.URL+"/doc.pdf", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(plain, "Content type application/pdf cannot be simplified to markdown") {
		t.Fatalf("default must keep the raw-content note for PDFs, got a different shape")
	}
}

func TestFetch_PDFHandlerSniffsMagic(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "application/octet-stream", []byte("%PDF-1.4\nbody"))
	out, err := Fetch(context.Background(), srv.URL, Options{
		PDFHandler: func(context.Context, []byte) (string, error) { return "SNIFFED", nil },
	})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.HasSuffix(out, "\nSNIFFED") {
		t.Fatalf("a %%PDF- body must reach the handler, got:\n%s", out)
	}
}

func TestFetch_PDFHandlerIgnoresLabelWithoutMagic(t *testing.T) {
	allowLoopback(t)
	// The Content-Type alone does not make a PDF: no magic, no handler.
	srv := serve(t, "Application/PDF", []byte("not a pdf"))
	called := false
	out, err := Fetch(context.Background(), srv.URL, Options{
		PDFHandler: func(context.Context, []byte) (string, error) { called = true; return "x", nil },
	})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if called || !strings.Contains(out, "cannot be simplified") {
		t.Fatalf("a body without %%PDF- must not reach the handler, got:\n%s", out)
	}
}

func TestFetch_PDFHandlerPanic(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "application/pdf", []byte("%PDF-1.4\nbody"))
	_, err := Fetch(context.Background(), srv.URL, Options{
		PDFHandler: func(context.Context, []byte) (string, error) { panic("bad xref") },
	})
	if err == nil || !strings.Contains(err.Error(), "Failed to extract PDF") || !strings.Contains(err.Error(), "bad xref") {
		t.Fatalf("a handler panic must become an error, got: %v", err)
	}
}

func TestFetch_EscapeHatch(t *testing.T) {
	allowLoopback(t)
	page := `<!doctype html><html lang="en"><body>
		<nav>NAVMARKER menu</nav>
		<div class="ad">ADMARKER buy now</div>
		<article id="main"><h1>Heading</h1><p>ARTICLEMARKER the primary body text, long enough to be the main content of the page.</p>
		<p class="drop">DROPMARKER a removable sentence with enough words that readability keeps it by default.</p></article>
		<footer>FOOTERMARKER copyright</footer>
		</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	// FullPage: skips Readability, so nav/footer that Readability drops survive.
	full, err := Fetch(context.Background(), srv.URL+"/p", Options{FullPage: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	for _, want := range []string{"NAVMARKER", "ARTICLEMARKER", "FOOTERMARKER"} {
		if !strings.Contains(full, want) {
			t.Fatalf("FullPage missing %q, got:\n%s", want, full)
		}
	}

	// Selector: only the matched subtree is converted.
	only, err := Fetch(context.Background(), srv.URL+"/p", Options{Selector: "#main"})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(only, "ARTICLEMARKER") {
		t.Fatalf("Selector should keep the target, got:\n%s", only)
	}
	if strings.Contains(only, "NAVMARKER") || strings.Contains(only, "FOOTERMARKER") {
		t.Fatalf("Selector should exclude non-target content, got:\n%s", only)
	}

	// ExcludeSelectors composes with FullPage: the ad is stripped, the rest stays.
	noAd, err := Fetch(context.Background(), srv.URL+"/p", Options{FullPage: true, ExcludeSelectors: []string{".ad"}})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(noAd, "ADMARKER") {
		t.Fatalf("ExcludeSelectors should remove the ad, got:\n%s", noAd)
	}
	if !strings.Contains(noAd, "ARTICLEMARKER") {
		t.Fatalf("ExcludeSelectors removed too much, got:\n%s", noAd)
	}

	// ExcludeSelectors composes with the DEFAULT Readability path (no FullPage/
	// Selector): the pruned element is gone though Readability would keep it.
	def, err := Fetch(context.Background(), srv.URL+"/p", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(def, "DROPMARKER") {
		t.Fatalf("precondition: default Readability path should keep the drop paragraph, got:\n%s", def)
	}
	// A blank Selector (an untrimmed tool argument) is the default path.
	blank, err := Fetch(context.Background(), srv.URL+"/p", Options{Selector: " "})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if blank != def {
		t.Fatalf("blank Selector should match the default output, got:\n%s", blank)
	}
	excl, err := Fetch(context.Background(), srv.URL+"/p", Options{ExcludeSelectors: []string{".drop"}})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(excl, "DROPMARKER") {
		t.Fatalf("ExcludeSelectors should compose with the Readability path, got:\n%s", excl)
	}
	if !strings.Contains(excl, "ARTICLEMARKER") {
		t.Fatalf("ExcludeSelectors removed too much on the Readability path, got:\n%s", excl)
	}

	// No selector match: sentinel content, and importantly a nil error (so a
	// caller's fallback is not triggered by a mistyped selector).
	miss, err := Fetch(context.Background(), srv.URL+"/p", Options{Selector: "#does-not-exist"})
	if err != nil {
		t.Fatalf("no-match must not error (would misfire the fallback): %v", err)
	}
	if !strings.Contains(miss, "<error>No content matched the selector.</error>") {
		t.Fatalf("expected no-match sentinel, got:\n%s", miss)
	}

	// A rejected selector is reported as such, not as a miss (Selector)
	// or ignored (ExcludeSelectors); no page content, nil error.
	for _, tc := range []struct {
		opts Options
		want string
	}{
		{Options{Selector: "div["}, `<error>Unsupported or invalid selector "div[": `},
		{Options{ExcludeSelectors: []string{`a[href="x\"`}}, `<error>Unsupported or invalid exclude selector "a[href="x\"": `},
		// The caller's input can't break the one-line <error>…</error>.
		{Options{Selector: "div\n\r\t\x00["}, `<error>Unsupported or invalid selector "div    [": `},
		{Options{Selector: "p</error><x>["}, `<error>Unsupported or invalid selector "p‹/error›‹x›[": `},
		{Options{ExcludeSelectors: []string{"#<"}}, `<error>Unsupported or invalid exclude selector "#‹": `},
		{Options{Selector: strings.Repeat("a", 300) + "["}, `<error>Unsupported or invalid selector "` + strings.Repeat("a", 200) + `…": `},
	} {
		out, err := Fetch(context.Background(), srv.URL+"/p", tc.opts)
		if err != nil {
			t.Fatalf("%+v: rejected selector must not error: %v", tc.opts, err)
		}
		// The reason is the CSS parser's wording, not pinned here.
		head := "Contents of " + srv.URL + "/p:\n"
		if want := head + tc.want; !strings.HasPrefix(out, want) || !strings.HasSuffix(out, ".</error>") || strings.Count(out, "\n") != 1 {
			t.Fatalf("%+v: want %q…, got:\n%q", tc.opts, want, out)
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(out, head+"<error>"), "</error>")
		if strings.ContainsAny(inner, "<>\x00\r\t") {
			t.Fatalf("%+v: raw markup or control character inside the sentinel: %q", tc.opts, out)
		}
	}
}

func TestExtract_InvalidSelector(t *testing.T) {
	page := `<!doctype html><html><body>
		<article><h1>Heading</h1><p>KEEP the primary body text, long enough to be the main content of the page.</p>
		<p class="ad">AD a removable sentence with enough words that readability keeps it by default.</p></article>
		</body></html>`
	for _, tc := range []struct {
		opts Options
		want string
	}{
		{Options{Selector: "div["}, `Unsupported or invalid selector "div[": `},
		{Options{Selector: "#"}, `Unsupported or invalid selector "#": `},
		{Options{Selector: "::before"}, `Unsupported or invalid selector "::before": `},
		// One bad member fails the whole group, though the other would match.
		{Options{Selector: "article, p["}, `Unsupported or invalid selector "article, p[": `},
		{Options{FullPage: true, Selector: "article >"}, `Unsupported or invalid selector "article ›": `},
		// Valid CSS cascadia doesn't support gets the same sentinel.
		{Options{Selector: ":is(article)"}, `Unsupported or invalid selector ":is(article)": `},
		// Every mode an exclude composes with: Readability, FullPage, Selector.
		{Options{ExcludeSelectors: []string{"p["}}, `Unsupported or invalid exclude selector "p[": `},
		{Options{FullPage: true, ExcludeSelectors: []string{".ad", " ", "p:nope"}}, `Unsupported or invalid exclude selector "p:nope": `},
		{Options{Selector: "article", ExcludeSelectors: []string{".ad, p["}}, `Unsupported or invalid exclude selector ".ad, p[": `},
		// Excludes are checked first; the selector is the normalized one.
		{Options{Selector: "p[", ExcludeSelectors: []string{"x" + string(rune(0xAD)) + "["}}, `Unsupported or invalid exclude selector "x[": `},
	} {
		out := extractContentFromHTML(page, nil, tc.opts)
		if !strings.HasPrefix(out, "<error>"+tc.want) || !strings.HasSuffix(out, ".</error>") || strings.Contains(out, "KEEP") {
			t.Errorf("%+v: want %q sentinel only, got:\n%q", tc.opts, tc.want, out)
		}
		if out == errNoSelector {
			t.Errorf("%+v: rejected selector reported as no match", tc.opts)
		}
	}
	// Valid selectors still match, the group and the exclude included.
	if out := extractContentFromHTML(page, nil, Options{Selector: "article, h1", ExcludeSelectors: []string{"x, .ad"}}); out != "# Heading\n\nKEEP the primary body text, long enough to be the main content of the page." {
		t.Errorf("valid selectors: unexpected result:\n%q", out)
	}
}

func TestSelectorReason(t *testing.T) {
	// Leftover input: cascadia's message repeats the selector Go-quoted
	// (doubled backslashes); the sentinel shows it once, as written.
	sel := `#a\ b)`
	_, err := cascadia.Compile(sel)
	if err == nil {
		t.Fatal("precondition: want a parse error")
	}
	if got := selectorReason(sel, err); strings.Contains(got, "parsing") || strings.Contains(got, `\\`) || got == "" {
		t.Errorf("selectorReason(%q) = %q, want the message without the quoted selector", sel, got)
	}
	// Other messages pass through.
	if got := selectorReason("x", errors.New("expected identifier")); got != "expected identifier" {
		t.Errorf("selectorReason: got %q", got)
	}
}

func TestSelectorShown(t *testing.T) {
	long := strings.Repeat("é", 250)
	for _, tc := range []struct{ in, want string }{
		{"div\np", "div p"},
		{"a\x00b\r\tc\u0085d", "a b  c d"},
		{`p</error><error>x`, `p‹/error›‹error›x`},
		{"article > p", "article › p"},
		// CSS escapes stay as written, not Go-quoted.
		{`#a\ b, a[href="x\"y"], .\31 0`, `#a\ b, a[href="x\"y"], .\31 0`},
		{long, strings.Repeat("é", 200) + "…"},
		{long[:400], long[:400]}, // exactly 200 runes: not cut
		{"", ""},
	} {
		if got := selectorShown(tc.in); got != tc.want {
			t.Errorf("selectorShown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFetch_RawSkipsSimplification(t *testing.T) {
	allowLoopback(t)
	body := `<!doctype html><html><body><article><h1>Hi</h1><p>Body body body body body body body body body.</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/p", Options{Raw: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "<article>") {
		t.Fatalf("raw mode should return unsimplified HTML, got:\n%s", out)
	}
}

func TestFetch_NonHTMLPrefix(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"key":"value"}`)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/data.json", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "Content type application/json cannot be simplified to markdown, but here is the raw content:") {
		t.Fatalf("expected non-simplifiable prefix, got:\n%s", out)
	}
	if !strings.Contains(out, `{"key":"value"}`) {
		t.Fatalf("expected raw json body, got:\n%s", out)
	}
}

func TestFetch_Truncation(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ABCDEFGHIJ") // 10 chars
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/t", Options{MaxLength: 4, StartIndex: 0})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "ABCD") {
		t.Fatalf("expected first 4 chars, got:\n%s", out)
	}
	if !strings.Contains(out, "start_index of 4") {
		t.Fatalf("expected truncation continuation note, got:\n%s", out)
	}

	// start past the end -> no more content
	out2, err := Fetch(context.Background(), srv.URL+"/t", Options{MaxLength: 4, StartIndex: 999})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out2, "<error>No more content available.</error>") {
		t.Fatalf("expected no-more-content error, got:\n%s", out2)
	}
}

func TestFetch_NoRobotsEnforcement(t *testing.T) {
	allowLoopback(t)
	// robots.txt forbids everything; webfetch must ignore it entirely and never
	// even request /robots.txt.
	robotsRequested := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsRequested = true
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/anything", Options{})
	if err != nil {
		t.Fatalf("Fetch must not enforce robots.txt, got: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected body, got:\n%s", out)
	}
	if robotsRequested {
		t.Fatal("webfetch must not request /robots.txt")
	}
}

func TestFetch_HTTPError(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/boom", Options{})
	if err == nil || !strings.Contains(err.Error(), "status code 500") {
		t.Fatalf("expected status-code error, got: %v", err)
	}
}

func TestSSRF_GuardAllowsOnlyPublic(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80",          // loopback
		"[::1]:80",              // loopback v6
		"10.1.2.3:443",          // RFC1918
		"192.168.1.1:80",        // RFC1918
		"172.16.0.1:80",         // RFC1918
		"100.64.0.1:80",         // CGNAT
		"169.254.169.254:80",    // link-local metadata endpoint
		"0.0.0.0:80",            // this-host
		"255.255.255.255:80",    // broadcast
		"224.0.0.1:80",          // multicast
		"198.18.0.1:80",         // benchmarking
		"192.0.2.1:80",          // TEST-NET-1
		"203.0.113.5:80",        // TEST-NET-3
		"[fc00::1]:80",          // IPv6 ULA
		"[fe80::1]:80",          // IPv6 link-local
		"[2001:db8::1]:80",      // IPv6 documentation
		"[::ffff:127.0.0.1]:80", // IPv4-mapped loopback (bypass attempt)
		"[::127.0.0.1]:80",      // IPv4-compatible loopback
		"[64:ff9b::7f00:1]:80",  // NAT64 of 127.0.0.1
		"[2002:7f00:1::1]:80",   // 6to4 of 127.0.0.1
		"[fec0::1]:80",          // IPv6 site-local (deprecated)
		"[2001:2::1]:80",        // IPv6 benchmarking
		"[::ffff:0:7f00:1]:80",  // IPv4-translated (SIIT) 127.0.0.1
		"[2001::7f00:1]:80",     // Teredo
		"[2001:10::1]:80",       // ORCHID
		"[2001:20::1]:80",       // ORCHIDv2
		"[100:0:0:1::1]:80",     // dummy prefix
		"[4000::1]:80",          // unallocated (outside 2000::/3)
		"[1::1]:80",             // unallocated (outside 2000::/3)
	}
	for _, addr := range blocked {
		// Rejected by the public-address rule, not by some parse failure.
		if err := GuardedControl("tcp", addr, nil); err == nil || !strings.Contains(err.Error(), "non-public") {
			t.Errorf("expected %s to be blocked as non-public, got: %v", addr, err)
		}
	}
	allowed := []string{"1.1.1.1:443", "8.8.8.8:53", "93.184.216.34:80", "[2606:4700:4700::1111]:443", "[2a00:1450:4001::1]:443", "[2400:cb00::1]:443"}
	for _, addr := range allowed {
		if err := GuardedControl("tcp", addr, nil); err != nil {
			t.Errorf("expected %s to be allowed, got: %v", addr, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   net.IP
		want bool
	}{
		{net.ParseIP("1.1.1.1"), true},
		{net.ParseIP("2606:4700:4700::1111"), true},
		{net.ParseIP("127.0.0.1"), false},
		{net.ParseIP("::ffff:10.0.0.1"), false},
		{net.ParseIP("2001:db8::1"), false},
		{nil, false},
		{net.IP{1, 2, 3}, false},
	}
	for _, c := range cases {
		if got := IsPublicIP(c.ip); got != c.want {
			t.Errorf("IsPublicIP(%v) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestFetch_SSRFBlocksLoopbackEndToEnd(t *testing.T) {
	// Guard active (not relaxed): a loopback target must be refused at dial.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "should never be reached")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/x", Options{})
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("expected SSRF dial refusal, got: %v", err)
	}
}

// compile-time nod that GuardedControl matches the Dialer.Control signature.
var _ func(string, string, syscall.RawConn) error = GuardedControl

// The error paths below were previously untested. They assert errors.Is
// unwrapping too: fetchURL wraps with %w, and a caller distinguishing a
// context cancellation from a genuine transport failure depends on that.

func TestFetch_InvalidURLRejected(t *testing.T) {
	allowLoopback(t)
	// A control character in the URL fails http.NewRequestWithContext before
	// any connection is attempted.
	_, err := Fetch(context.Background(), "http://exa\x7fmple.com", Options{})
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
	if !strings.Contains(err.Error(), "Failed to fetch") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestFetch_ConnectionRefused(t *testing.T) {
	allowLoopback(t)
	// Bind then close, so the port is almost certainly free and refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	_, err = Fetch(context.Background(), "http://"+addr, Options{})
	if err == nil {
		t.Fatal("expected an error when the connection is refused")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("transport error was not unwrappable, got: %v", err)
	}
}

func TestFetch_TruncatedBodyReportsReadFailure(t *testing.T) {
	allowLoopback(t)
	// Content-Length promises 1024 bytes, the handler writes 7 and returns, so
	// the connection closes short. client.Do has already succeeded by then
	// (headers and the first bytes are in the socket buffer), which puts the
	// failure in io.ReadAll deterministically rather than racing the dial.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "1024")
		if _, werr := w.Write([]byte("partial")); werr != nil {
			return
		}
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, Options{})
	if err == nil {
		t.Fatal("expected an error when the body is truncated")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body-read error was not unwrappable, got: %v", err)
	}
}

func TestFetch_PDFHandlerError(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "application/pdf", []byte("%PDF-1.4\nbody"))
	boom := errors.New("tika down")
	_, err := Fetch(context.Background(), srv.URL, Options{
		PDFHandler: func(context.Context, []byte) (string, error) { return "", boom },
	})
	if err == nil || !strings.Contains(err.Error(), "Failed to extract PDF") || !errors.Is(err, boom) {
		t.Fatalf("want a wrapped Failed to extract PDF error, got: %v", err)
	}
}

func TestFetch_PDFHandlerEmptyText(t *testing.T) {
	allowLoopback(t)
	// A scan without OCR yields no text; that must surface as an error so a
	// caller's fallback runs.
	srv := serve(t, "application/pdf", []byte("%PDF-1.4\nbody"))
	_, err := Fetch(context.Background(), srv.URL, Options{
		PDFHandler: func(context.Context, []byte) (string, error) { return " \n ", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "no extractable text") {
		t.Fatalf("want a no extractable text error, got: %v", err)
	}
}

func TestFetch_PDFCap(t *testing.T) {
	allowLoopback(t)
	handler := func(context.Context, []byte) (string, error) { return "ok", nil }

	// A PDF over the 10 MiB body cap passes with a handler (50 MiB PDF cap)...
	big := append([]byte("%PDF-1.4\n"), make([]byte, DefaultMaxBodyBytes+1)...)
	srv := serve(t, "application/pdf", big)
	if _, err := Fetch(context.Background(), srv.URL, Options{PDFHandler: handler}); err != nil {
		t.Fatalf("PDF under the PDF cap must pass, got: %v", err)
	}
	// ...but not without one: the body cap still applies.
	if _, err := Fetch(context.Background(), srv.URL, Options{}); err == nil {
		t.Fatal("without a handler the body cap must apply")
	}
	// The magic bytes, not the label, pick the cap: a sniffed PDF gets it...
	sniffed := serve(t, "application/octet-stream", big)
	if _, err := Fetch(context.Background(), sniffed.URL, Options{PDFHandler: handler}); err != nil {
		t.Fatalf("a sniffed PDF must get the PDF cap, got: %v", err)
	}
	// ...and a body merely labelled application/pdf does not.
	junk := make([]byte, 200)
	if _, err := Fetch(context.Background(), serve(t, "application/pdf", junk).URL, Options{PDFHandler: handler, MaxBodyBytes: 100}); err == nil {
		t.Fatal("a mislabelled body must keep MaxBodyBytes")
	}

	// Over the PDF cap fails, whether announced by Content-Length or streamed.
	small := Options{PDFHandler: handler, MaxPDFBytes: 100}
	body := append([]byte("%PDF-1.4\n"), make([]byte, 200)...)
	if _, err := Fetch(context.Background(), serve(t, "application/pdf", body).URL, small); err == nil ||
		!strings.Contains(err.Error(), "exceeds 100 bytes") {
		t.Fatalf("Content-Length over the PDF cap must fail, got: %v", err)
	}
	streamed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.(http.Flusher).Flush() // no Content-Length: chunked
		_, _ = w.Write(body)
	}))
	defer streamed.Close()
	if _, err := Fetch(context.Background(), streamed.URL, small); err == nil ||
		!strings.Contains(err.Error(), "exceeds 100 bytes") {
		t.Fatalf("a streamed body over the PDF cap must fail, got: %v", err)
	}

	// HTML keeps the body cap even with a handler set.
	html := []byte("<html><body>" + strings.Repeat("x", 200) + "</body></html>")
	if _, err := Fetch(context.Background(), serve(t, "text/html", html).URL, Options{PDFHandler: handler, MaxBodyBytes: 100}); err == nil {
		t.Fatal("HTML must keep MaxBodyBytes when a PDF handler is set")
	}
}

// serve returns an httptest server that answers every request with the given
// content type and body.
func serve(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_UndeclaredCharsetIsUTF8(t *testing.T) {
	allowLoopback(t)
	// Pure ASCII for the first KiB (the charset sniffing window), then UTF-8.
	// Without a charset the old windows-1252 fallback turned "é" into "Ã©".
	body := append(bytes.Repeat([]byte("a"), 1200), "é ✓"...)
	for _, ct := range []string{"text/plain", "application/json", "text/html", ""} {
		srv := serve(t, ct, body)
		out, err := Fetch(context.Background(), srv.URL+"/x", Options{Raw: true, MaxLength: 2000})
		if err != nil {
			t.Fatalf("[%q] Fetch error: %v", ct, err)
		}
		if !strings.Contains(out, "é ✓") {
			t.Fatalf("[%q] expected UTF-8 to survive, got tail: %q", ct, out[len(out)-20:])
		}
	}
}

func TestFetch_DeclaredAndSniffedLegacyCharset(t *testing.T) {
	allowLoopback(t)
	latin1 := []byte("caf\xe9") // "café" in ISO-8859-1 / windows-1252
	cases := map[string]string{
		"text/plain; charset=iso-8859-1": "header-declared charset",
		"text/plain":                     "undeclared, not valid UTF-8: library guess (windows-1252)",
	}
	for ct, why := range cases {
		srv := serve(t, ct, latin1)
		out, err := Fetch(context.Background(), srv.URL+"/x", Options{})
		if err != nil {
			t.Fatalf("[%s] Fetch error: %v", why, err)
		}
		if !strings.Contains(out, "café") {
			t.Fatalf("[%s] expected transcoded text, got:\n%s", why, out)
		}
	}
	// A meta charset on an HTML page, no header charset.
	srv := serve(t, "text/html", []byte(`<html><head><meta charset="iso-8859-1"></head><body><p>caf`+"\xe9"+`</p></body></html>`))
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{Raw: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "café") {
		t.Fatalf("expected meta charset to be honoured, got:\n%s", out)
	}
}

func TestFetch_EmptyBody(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "text/plain", nil)
	out, err := Fetch(context.Background(), srv.URL+"/empty", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	want := "Content type text/plain cannot be simplified to markdown, but here is the raw content:\nContents of " + srv.URL + "/empty:\n<error>No more content available.</error>"
	if out != want {
		t.Fatalf("unexpected output for empty body:\n%s", out)
	}
}

func TestFetch_SelectorPathLinksResolveAgainstPage(t *testing.T) {
	allowLoopback(t)
	page := `<!doctype html><html><body><div id="c">
		<a href="/root">root</a>
		<a href="rel.html">rel</a>
		<a href="../up.html">up</a>
		<img src="//cdn.example/i.png" alt="i">
		<a href="mailto:a@example.com">mail</a>
		<a href="https://other.example/abs">abs</a>
		<a href="data:text/plain,hi">data</a>
		</div></body></html>`
	srv := serve(t, "text/html; charset=utf-8", []byte(page))

	for _, opts := range []Options{{Selector: "#c"}, {FullPage: true}} {
		out, err := Fetch(context.Background(), srv.URL+"/dir/sub/page", opts)
		if err != nil {
			t.Fatalf("Fetch error: %v", err)
		}
		for _, want := range []string{
			"(" + srv.URL + "/root)",
			"(" + srv.URL + "/dir/sub/rel.html)",
			"(" + srv.URL + "/dir/up.html)",
			"(http://cdn.example/i.png)", // protocol-relative takes the page scheme
			"(mailto:a@example.com)",
			"(https://other.example/abs)",
			"(data:text/plain,hi)",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("%+v: expected %q in:\n%s", opts, want, out)
			}
		}
	}
}

func TestAbsolutizeLinks_HTTPSBase(t *testing.T) {
	// The converter's own domain handling forced "http"; ours keeps the scheme.
	base, _ := url.Parse("https://example.com/a/b/page.html?q=1")
	doc, err := goqueryDoc(`<a href="/x">x</a><a href="y">y</a><img src="//h/i.png"><a href="#frag">f</a><a href="">e</a><a href="javascript:void(0)">j</a>`)
	if err != nil {
		t.Fatal(err)
	}
	absolutizeLinks(doc, base)
	got := []string{}
	doc.Find("[href], [src]").Each(func(_ int, s *goquery.Selection) {
		if v, ok := s.Attr("href"); ok {
			got = append(got, v)
		}
		if v, ok := s.Attr("src"); ok {
			got = append(got, v)
		}
	})
	want := []string{
		"https://example.com/x",
		"https://example.com/a/b/y",
		"https://h/i.png",
		"#frag", // in-page anchors stay relative, as on the Readability path
		"",
		"javascript:void(0)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// nil base: untouched.
	doc2, _ := goqueryDoc(`<a href="/x">x</a>`)
	absolutizeLinks(doc2, nil)
	if v, _ := doc2.Find("a").Attr("href"); v != "/x" {
		t.Fatalf("nil base must be a no-op, got %q", v)
	}
}

func goqueryDoc(fragment string) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(strings.NewReader(fragment))
}

func TestFetch_RedirectMovesLinkBaseNotWrapper(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new/dir/page", http.StatusFound)
		case "/new/dir/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><body><div id="c"><a href="rel.html">rel</a></div></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/old", Options{Selector: "#c"})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.HasPrefix(out, "Contents of "+srv.URL+"/old:\n") {
		t.Fatalf("wrapper must keep the requested URL, got:\n%s", out)
	}
	if !strings.Contains(out, "("+srv.URL+"/new/dir/rel.html)") {
		t.Fatalf("relative link must resolve against the final URL, got:\n%s", out)
	}
}

func TestFetch_ExcludeSelectorsWithReadabilityBase(t *testing.T) {
	allowLoopback(t)
	// ExcludeSelectors-only path now hands the parsed tree to Readability
	// directly; links must still be absolutized via base as on the plain path.
	page := `<!doctype html><html><body><article><h1>T</h1>
		<p class="drop">DROPMARKER enough words here that readability keeps this paragraph around by default.</p>
		<p>KEEPMARKER the primary body text, long enough to be considered the main content of the page. See <a href="/link">the link</a> for more.</p>
		</article></body></html>`
	srv := serve(t, "text/html; charset=utf-8", []byte(page))
	out, err := Fetch(context.Background(), srv.URL+"/p", Options{ExcludeSelectors: []string{".drop"}})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(out, "DROPMARKER") || !strings.Contains(out, "KEEPMARKER") {
		t.Fatalf("unexpected pruning result:\n%s", out)
	}
	if !strings.Contains(out, "("+srv.URL+"/link)") {
		t.Fatalf("expected absolutized link, got:\n%s", out)
	}
}

func TestExtract_ExcludeSelectorsNormalized(t *testing.T) {
	// The Readability path NFC-normalizes the page and strips soft hyphens, so
	// selectors are normalized too: NFD / soft-hyphen text in them still
	// matches.
	page := `<!doctype html><html><body><article><h1>T</h1>
		<p title="cafe` + "\u0301" + `">DROPNFD enough words here that readability keeps this paragraph around by default.</p>
		<p class="x">DROPSHY Co` + "\u00ad" + `op enough words here that readability keeps this paragraph around too.</p>
		<p>KEEPMARKER the primary body text, long enough to be considered the main content of the page. Caf` + "e\u0301" + ` Co` + "\u00ad" + `op.</p>
		</article></body></html>`
	out := extractContentFromHTML(page, nil, Options{ExcludeSelectors: []string{
		`p[title="cafe` + "\u0301" + `"]`,
		`p:contains("Co` + "\u00ad" + `op enough")`,
	}})
	if strings.Contains(out, "DROPNFD") || strings.Contains(out, "DROPSHY") || !strings.Contains(out, "KEEPMARKER") {
		t.Fatalf("unexpected pruning result:\n%s", out)
	}
	if !strings.Contains(out, "Caf\u00e9 Coop.") {
		t.Fatalf("expected NFC text without soft hyphens, got:\n%q", out)
	}
}

func TestExtract_SelectorFullPageNormalized(t *testing.T) {
	// Selector and FullPage normalize like the Readability path: NFC, soft
	// hyphens removed. The fixture carries literal NFD accents and U+00AD.
	raw, err := os.ReadFile("testdata/readability/literal-shy-nfd.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	if !strings.Contains(page, "\u00ad") || norm.NFC.IsNormalString(page) {
		t.Fatal("fixture lost its soft hyphens / NFD text")
	}
	for _, opts := range []Options{{Selector: "article"}, {FullPage: true}} {
		out := extractContentFromHTML(page, nil, opts)
		if strings.Contains(out, "\u00ad") || !norm.NFC.IsNormalString(out) {
			t.Errorf("%+v: expected NFC text without soft hyphens, got:\n%q", opts, out)
		}
		if !strings.Contains(out, "R\u00e9publique") || !strings.Contains(out, "travaux") || !strings.Contains(out, "commer\u00e7ants") {
			t.Errorf("%+v: normalized words missing:\n%q", opts, out)
		}
	}
}

func TestExtract_SelectorNormalized(t *testing.T) {
	// The tree is normalized, so the selector is too: NFD / soft-hyphen text
	// in it still matches the page's literal (now normalized) characters.
	page := `<!doctype html><html><body>
		<p title="cafe` + "\u0301" + `">NFDMATCH</p>
		<p>SHYMATCH Co` + "\u00ad" + `op</p>
		<p>OTHER</p>
		</body></html>`
	for sel, want := range map[string]string{
		`p[title="cafe` + "\u0301" + `"]`:    "NFDMATCH",
		`p:contains("Co` + "\u00ad" + `op")`: "SHYMATCH",
	} {
		out := extractContentFromHTML(page, nil, Options{Selector: sel})
		if !strings.Contains(out, want) || strings.Contains(out, "OTHER") {
			t.Errorf("Selector %q: unexpected result:\n%s", sel, out)
		}
		out = extractContentFromHTML(page, nil, Options{FullPage: true, ExcludeSelectors: []string{sel}})
		if strings.Contains(out, want) || !strings.Contains(out, "OTHER") {
			t.Errorf("ExcludeSelectors %q: unexpected result:\n%s", sel, out)
		}
	}
}

func TestExtract_SelectorNormalizedOneForm(t *testing.T) {
	// Only the normalized selector is matched: text written as a reference
	// stays unnormalized in the tree and is not matched.
	page := `<!doctype html><html><body>
		<p title="cafe` + "\u0301" + `">LIT1</p>
		<p title="cafe&#x301;">REF2</p>
		<p title="cafe` + "\u0301" + `">LIT3</p>
		</body></html>`
	sel := `p[title="cafe` + "\u0301" + `"]`
	if out := extractContentFromHTML(page, nil, Options{Selector: sel}); out != "LIT1\n\nLIT3" {
		t.Errorf("Selector: want LIT1, LIT3, got:\n%q", out)
	}
	if out := extractContentFromHTML(page, nil, Options{ExcludeSelectors: []string{sel}}); !strings.Contains(out, "REF2") || strings.Contains(out, "LIT1") || strings.Contains(out, "LIT3") {
		t.Errorf("ExcludeSelectors: want only REF2, got:\n%q", out)
	}
	// Matching the original form too would invert a negation: "Coop" passes
	// :not(:contains("Co\u00adop")).
	page = `<!doctype html><html><body><article>
		<p>KEEP Co` + "\u00ad" + `op</p>
		<p title="cafe` + "\u0301" + `">KEEP2</p>
		<p>OTHER</p>
		</article></body></html>`
	sel = `p:not(:contains("Co` + "\u00ad" + `op"))`
	if out := extractContentFromHTML(page, nil, Options{Selector: sel}); out != "KEEP2\n\nOTHER" {
		t.Errorf("negated Selector: want KEEP2, OTHER, got:\n%q", out)
	}
	if out := extractContentFromHTML(page, nil, Options{ExcludeSelectors: []string{sel}}); !strings.Contains(out, "KEEP Coop") || strings.Contains(out, "OTHER") {
		t.Errorf("negated ExcludeSelectors: want only KEEP Coop, got:\n%q", out)
	}
}

func TestExtract_SelectorWhitespace(t *testing.T) {
	// No-break space (not CSS whitespace, part of a name) and soft hyphen.
	nbsp, shy := string(rune(0xA0)), string(rune(0xAD))
	page := `<!doctype html><html><body>
		<p id="a">AAA</p>
		<p id="s ">SSS</p>
		<p id="a&nbsp;">NBSP</p>
		<p>OTHER</p>
		</body></html>`
	full := extractContentFromHTML(page, nil, Options{FullPage: true})
	// Blank Selector is unset: FullPage applies as if it were "".
	if out := extractContentFromHTML(page, nil, Options{FullPage: true, Selector: " \t\r\f"}); out != full {
		t.Errorf("blank Selector with FullPage: want FullPage output, got:\n%q", out)
	}
	// Selectors reach the CSS parser as given: it skips CSS-whitespace
	// padding, an escaped trailing space and a no-break space are part of the id.
	for sel, want := range map[string]string{
		" #a\n": "AAA", "#s\\ ": "SSS", " #a , #s\\  ": "AAA\n\nSSS", "#a" + nbsp: "NBSP",
	} {
		if out := extractContentFromHTML(page, nil, Options{Selector: sel}); out != want {
			t.Errorf("Selector %q: want %q, got %q", sel, want, out)
		}
		out := extractContentFromHTML(page, nil, Options{FullPage: true, ExcludeSelectors: []string{sel}})
		for _, w := range strings.Split(want, "\n\n") {
			if strings.Contains(out, w) || !strings.Contains(out, "OTHER") {
				t.Errorf("ExcludeSelectors %q: want %q removed, got:\n%q", sel, w, out)
			}
		}
	}
	// Blank entries are dropped, blank after normalizing (soft hyphen) too.
	if out := extractContentFromHTML(page, nil, Options{FullPage: true, ExcludeSelectors: []string{"", " ", "\n", shy}}); out != full {
		t.Errorf("blank ExcludeSelectors: want FullPage output, got:\n%q", out)
	}
	if out := extractContentFromHTML(page, nil, Options{FullPage: true, Selector: " " + shy + " "}); out != full {
		t.Errorf("soft-hyphen Selector: want FullPage output, got:\n%q", out)
	}
}

func TestCleanSelector(t *testing.T) {
	// Soft hyphen, no-break space, ideographic space.
	shy, nbsp, ideo := string(rune(0xAD)), string(rune(0xA0)), string(rune(0x3000))
	for in, want := range map[string]string{
		"":                "",
		" \t\n\r\f\v":     "",
		nbsp + ideo:       "",
		" " + shy + " ":   "",
		" #a ":            " #a ",
		"#a\\ ":           "#a\\ ",
		"#a" + nbsp:       "#a" + nbsp,
		"Co" + shy + "op": "Coop",
	} {
		if got := cleanSelector(in); got != want {
			t.Errorf("cleanSelector(%q) = %q, want %q", in, got, want)
		}
	}
	// A blank-only list becomes nil, so the default path runs, not the
	// ExcludeSelectors one.
	o := Options{Selector: nbsp, ExcludeSelectors: []string{" ", shy}}.cleanSelectors()
	if o.Selector != "" || o.ExcludeSelectors != nil {
		t.Errorf("cleanSelectors: want blanks unset, got %+v", o)
	}
	o = Options{ExcludeSelectors: []string{" ", ".ad", "\n"}}.cleanSelectors()
	if len(o.ExcludeSelectors) != 1 || o.ExcludeSelectors[0] != ".ad" {
		t.Errorf("cleanSelectors: want only .ad, got %q", o.ExcludeSelectors)
	}
}

func TestNormalizeText(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"plain caf\u00e9":       "plain caf\u00e9",
		"cafe\u0301 co\u00adop": "caf\u00e9 coop",
		"\u00ad":                "",
		"\u00e9\u0301 co\u00ad": "\u00e9\u0301 co",
		"a\xffb":                "a\ufffdb",
		"a\xff\u00adb":          "a\ufffdb",
	} {
		if got := normalizeText(in); got != want {
			t.Errorf("normalizeText(%q) = %q, want %q", in, got, want)
		}
		if got, err := io.ReadAll(readability.NormalizeText(strings.NewReader(in))); err != nil || string(got) != want {
			t.Errorf("NormalizeText(%q) = %q, %v; normalizeText disagrees", in, got, err)
		}
	}
}

func TestSliceRunes_MatchesRuneSlicing(t *testing.T) {
	inputs := []string{"", "abc", "héllo wörld ✓✓✓", "日本語のテキスト", "a"}
	for _, s := range inputs {
		r := []rune(s)
		for start := 0; start <= len(r)+1; start++ {
			for n := 1; n <= len(r)+2; n++ {
				var want string
				if start < len(r) {
					end := min(start+n, len(r))
					want = string(r[start:end])
				}
				got, gotN, total := sliceRunes(s, start, n)
				if got != want || gotN != len([]rune(want)) || total != len(r) {
					t.Fatalf("sliceRunes(%q, %d, %d) = (%q, %d, %d), want (%q, %d, %d)",
						s, start, n, got, gotN, total, want, len([]rune(want)), len(r))
				}
			}
		}
	}
}

func TestFirstRunes(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"ééé<html", 3, "ééé"},
		{"ééé<html", 4, "ééé<"},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		if got := firstRunes(c.s, c.n); got != c.want {
			t.Errorf("firstRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestFetch_HTMLSniffIsCharacterBased(t *testing.T) {
	allowLoopback(t)
	// 60 two-byte runes (120 bytes) precede "<html": within upstream's 100
	// *character* window, outside a 100 *byte* one. No content-type, so only the
	// sniff decides — but an empty content-type is itself an HTML signal
	// upstream, so use a non-HTML content type to isolate the sniff.
	body := strings.Repeat("é", 60) + "<html><body><article><p>SNIFFMARKER the main content of this page, long enough for readability to keep.</p></article></body></html>"
	srv := serve(t, "application/octet-stream", []byte(body))
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(out, "cannot be simplified") {
		t.Fatalf("expected the page to be sniffed as HTML, got:\n%s", out)
	}
	if !strings.Contains(out, "SNIFFMARKER") {
		t.Fatalf("expected extracted content, got:\n%s", out)
	}
}

func TestReadBody_Limit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100)
	if b, err := readBody(bytes.NewReader(body), 0); err != nil || len(b) != 100 {
		t.Fatalf("unlimited: got %d bytes, err %v", len(b), err)
	}
	if b, err := readBody(bytes.NewReader(body), 100); err != nil || len(b) != 100 {
		t.Fatalf("exactly at cap must pass: got %d bytes, err %v", len(b), err)
	}
	if _, err := readBody(bytes.NewReader(body), 99); err == nil || !strings.Contains(err.Error(), "exceeds 99 bytes") {
		t.Fatalf("one over cap must fail, got: %v", err)
	}
	// limit+1 must not overflow into a negative LimitReader size.
	if b, err := readBody(bytes.NewReader(body), math.MaxInt64); err != nil || len(b) != 100 {
		t.Fatalf("MaxInt64 must mean unlimited: got %d bytes, err %v", len(b), err)
	}
}

func TestFetch_MaxBodyBytes(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "text/plain", bytes.Repeat([]byte("x"), 100))

	_, err := Fetch(context.Background(), srv.URL+"/big", Options{MaxBodyBytes: 50})
	if err == nil || !strings.Contains(err.Error(), "Failed to fetch") || !strings.Contains(err.Error(), "exceeds 50 bytes") {
		t.Fatalf("expected body-cap error, got: %v", err)
	}
	for _, cap := range []int64{0, -1, 100} {
		if _, err := Fetch(context.Background(), srv.URL+"/big", Options{MaxBodyBytes: cap}); err != nil {
			t.Fatalf("MaxBodyBytes=%d should succeed for a 100-byte body: %v", cap, err)
		}
	}

	// A declared Content-Length over the cap is rejected before any body is
	// read at all.
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1000))
	}))
	defer declared.Close()
	_, err = Fetch(context.Background(), declared.URL+"/declared", Options{MaxBodyBytes: 500})
	if err == nil || !strings.Contains(err.Error(), "exceeds 500 bytes") {
		t.Fatalf("expected Content-Length pre-check to reject, got: %v", err)
	}

	// The default cap is real: one byte over DefaultMaxBodyBytes fails, and the
	// handler is stopped early rather than streamed to completion. Chunked (no
	// Content-Length), so this exercises the read-side cap, not the pre-check.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 10; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte("x"))
	}))
	defer huge.Close()
	_, err = Fetch(context.Background(), huge.URL+"/huge", Options{})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d bytes", DefaultMaxBodyBytes)) {
		t.Fatalf("expected default cap to trigger, got: %v", err)
	}

	// A negative cap is unlimited: the same over-default body is read whole.
	if got := (Options{MaxBodyBytes: -1}).withDefaults().MaxBodyBytes; got != -1 {
		t.Fatalf("withDefaults must keep a negative cap, got %d", got)
	}
	if _, err := Fetch(context.Background(), huge.URL+"/huge", Options{MaxBodyBytes: -1}); err != nil {
		t.Fatalf("MaxBodyBytes=-1 should read past the default cap: %v", err)
	}
}

func TestFetch_RedirectLimit(t *testing.T) {
	allowLoopback(t)
	// /hop/<n>/<target> redirects to n+1 until n == target, then serves text.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n, target int
		if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d/%d", &n, &target); err != nil {
			http.NotFound(w, r)
			return
		}
		if n < target {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d/%d", n+1, target), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "landed")
	}))
	defer srv.Close()

	// 15 hops exceeds net/http's default of 10; exactly 20 is httpx's limit
	// and must still be followed; 21 must not.
	for _, hops := range []int{15, maxRedirects} {
		out, err := Fetch(context.Background(), fmt.Sprintf("%s/hop/0/%d", srv.URL, hops), Options{})
		if err != nil || !strings.Contains(out, "landed") {
			t.Fatalf("%d redirects should be followed, got err=%v out=%q", hops, err, out)
		}
	}
	_, err := Fetch(context.Background(), fmt.Sprintf("%s/hop/0/%d", srv.URL, maxRedirects+1), Options{})
	if err == nil || !strings.Contains(err.Error(), "Failed to fetch") || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("%d redirects should fail, got: %v", maxRedirects+1, err)
	}
}

func TestFetch_ReusesConnections(t *testing.T) {
	allowLoopback(t)
	var newConns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hi")
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	for i := 0; i < 3; i++ {
		if _, err := Fetch(context.Background(), srv.URL+"/r", Options{}); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("expected one pooled connection across sequential fetches, server saw %d", got)
	}
}

func TestFetch_SelectorNestedMatchesNotDuplicated(t *testing.T) {
	allowLoopback(t)
	page := `<!doctype html><html><body><div><div><p>only once</p></div></div><p>outside</p></body></html>`
	srv := serve(t, "text/html; charset=utf-8", []byte(page))
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{Selector: "div"})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if n := strings.Count(out, "only once"); n != 1 {
		t.Fatalf("expected nested match once, got %d in:\n%s", n, out)
	}
	if strings.Contains(out, "outside") {
		t.Fatalf("selector leaked non-matching content:\n%s", out)
	}
}

func TestFetch_SelectorPathDropsJavascriptLinks(t *testing.T) {
	allowLoopback(t)
	page := `<!doctype html><html><body><div id="c">
		<p><a href="javascript:void(0)">click me</a> and <a class="more" href=" JavaScript:go()"><b>bold</b> link</a></p>
		</div></body></html>`
	srv := serve(t, "text/html; charset=utf-8", []byte(page))
	for _, opts := range []Options{{Selector: "#c"}, {FullPage: true}} {
		out, err := Fetch(context.Background(), srv.URL+"/x", opts)
		if err != nil {
			t.Fatalf("Fetch error: %v", err)
		}
		if strings.Contains(strings.ToLower(out), "javascript:") {
			t.Fatalf("%+v: javascript: link survived:\n%s", opts, out)
		}
		if !strings.Contains(out, "click me") || !strings.Contains(out, "**bold** link") {
			t.Fatalf("%+v: link text lost:\n%s", opts, out)
		}
	}

	// A selector that targets the javascript: link itself still matches it.
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{Selector: "a.more"})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "**bold** link") || strings.Contains(out, "javascript:") {
		t.Fatalf("selector on a javascript: link: got:\n%s", out)
	}
}
