# webfetch

A dependency-light Go port of the reference Python
[`mcp-server-fetch`](https://github.com/modelcontextprotocol/servers/tree/main/src/fetch)
tool.

It fetches a URL, extracts the main content as Markdown (or returns raw HTML),
and returns text ready to hand to an LLM — all **in-process**, with no Node
subprocess and no sidecar container.

```go
out, err := webfetch.Fetch(ctx, "https://example.com/article", webfetch.Options{
    MaxLength:  5000, // default when 0
    StartIndex: 0,
    Raw:        false,
})
```

## What's different from upstream `mcp-server-fetch`

Same tool contract — identical `fetch` schema, the `Contents of <url>:` wrapper,
and the truncation / error strings — but:

- **In-process, no sidecar.** Runs directly inside your Go process: no Node
  subprocess, no Python runtime, no separate container to deploy or network-isolate.
- **Strict SSRF guard.** Reaches only globally-routable public IPs, enforced
  after DNS resolution in the dialer. Upstream explicitly warns it *can* reach
  local/internal addresses; here it cannot.
  → [SSRF protection](#ssrf-protection-only-public-ips-are-reachable)
- **PDF hand-off** (opt-in `PDFHandler`). Upstream returns PDFs as unusable raw
  bytes; webfetch can hand the bytes to your own extractor and return its text.
  → [Extension: `PDFHandler`](#extension-pdfhandler-off-by-default)
- **Content metadata** (opt-in `IncludeMetadata`). Optional
  title/author/date/site/language frontmatter — not offered upstream.
  → [Extension: `IncludeMetadata`](#extension-includemetadata-off-by-default)
- **Full-page & selector escape hatch** (opt-in `FullPage` / `Selector` /
  `ExcludeSelectors`). Steer or bypass Readability when it over-strips — upstream
  is main-content-or-raw only.
  → [Extension: escape hatch](#extension-full-page--selector-escape-hatch-off-by-default)
- **Pure-Go extraction pipeline.** `go-readability` + `html-to-markdown` in place
  of upstream's Node `readabilipy` + `markdownify`; output is byte-identical on
  typical pages. → [Fidelity](#fidelity)
- **Response body cap** (`MaxBodyBytes`, default 10 MiB). Upstream buffers
  bodies of any size; webfetch fails a fetch whose body is over the cap so a
  model-chosen URL cannot exhaust memory. → [Body cap](#body-cap-maxbodybytes-10-mib-by-default)
- **No proxy support.** Upstream's `--proxy-url` is intentionally unsupported: a
  proxy would move egress outside the guarded dialer, which is exactly where the
  SSRF check lives.

The opt-in extensions default to off, so unless you enable them the output stays
byte-identical to upstream. The body cap is the one default that is not
upstream's, and it only changes the outcome for bodies larger than the cap.

## Tool

The module exposes a single tool, equivalent to upstream `mcp-server-fetch`:

**`fetch`** — Fetches a URL from the internet and extracts its contents as
markdown.

| Parameter     | Type    | Required | Default | Description                                        |
| ------------- | ------- | -------- | ------- | -------------------------------------------------- |
| `url`         | string  | yes      | —       | URL to fetch                                       |
| `max_length`  | integer | no       | `5000`  | Maximum number of characters to return             |
| `start_index` | integer | no       | `0`     | Start content from this character index            |
| `raw`         | boolean | no       | `false` | Get raw content without markdown conversion        |

(`webfetch.Fetch` maps these to the `Options` fields `MaxLength`, `StartIndex`,
and `Raw`.)

### Extension: `IncludeMetadata` (off by default)

With `Options.IncludeMetadata: true`, the extracted Markdown is prefixed with a small YAML frontmatter block
built from metadata Readability already parses — `title`, `author`, `published`,
`site`, `language` (non-empty fields only):

```yaml
---
title: "The Article Title"
author: "Jane Doe"
published: "2024-01-02T15:04:05Z"
site: "Example Blog"
language: "en"
---
```

It applies only to the HTML-simplification path (not `Raw`, not non-HTML
content), and the frontmatter counts as part of the returned content, so
`StartIndex` / `MaxLength` page over it too. Left `false` (the default), output
is byte-identical to upstream — see [Fidelity](#fidelity).

### Extension: `PDFHandler` (off by default)

Upstream returns PDF responses as raw bytes behind a "cannot be simplified"
note, which is unusable as LLM context. webfetch does not parse PDFs itself;
set `Options.PDFHandler` and a PDF response (detected by the `%PDF-` magic
bytes, whatever its Content-Type) is handed to it as bytes, downloaded through
the same SSRF guard. The text it returns is the content, paged by `StartIndex` /
`MaxLength` like any other:

```go
// Illustrative: extractor stands for your own client, e.g. for a Tika sidecar.
out, err := webfetch.Fetch(ctx, url, webfetch.Options{
    PDFHandler: func(ctx context.Context, body []byte) (string, error) {
        ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
        defer cancel()
        return extractor.Extract(ctx, bytes.NewReader(body))
    },
})
```

The bytes are untrusted: run the parser where a malicious PDF cannot take your
process down (e.g. a Tika sidecar). The handler must bound its own run time;
webfetch's 30 s timeout covers only the download. With a handler set, PDF
bodies use their own cap, `MaxPDFBytes` (default `webfetch.DefaultMaxPDFBytes`,
50 MiB; negative disables it), independent of `MaxBodyBytes`. A handler error or
panic, or empty text (a scan without OCR), returns a `Failed to extract PDF`
error, so a caller's fallback can take over. `Raw` takes precedence. Nil (the
default) keeps the upstream raw-bytes behaviour.

### Extension: full-page & selector escape hatch (off by default)

When Readability over-strips (docs pages, tables, sidebars you actually want),
three opt-in `Options` let you bypass or steer it:

- `FullPage: true` — convert the **entire page** to Markdown, skipping the
  Readability main-content extraction.
- `Selector: "<css>"` — convert only the element(s) matching a CSS selector
  (takes precedence over `FullPage`). No match yields the content
  `<error>No content matched the selector.</error>` (with a nil error).
- `ExcludeSelectors: []string{"<css>", …}` — remove matching element(s) before
  conversion. Unlike the other two it **composes with every mode**, including the
  default Readability path (e.g. strip a cookie banner, then simplify).

`Raw` still takes precedence over all three, and `IncludeMetadata` is not applied
on the `FullPage` / `Selector` paths. Relative links are resolved against the
final URL (after redirects), keeping the page's scheme and directory, as on the
Readability path. Text is normalized as on the Readability path (NFC, soft
hyphens removed) and selectors with it; text the page writes as a reference
(`&shy;`, `&#x301;`) stays as written. A blank (whitespace only once normalized)
`Selector` or `ExcludeSelectors` entry counts as unset; others go to the CSS
parser as given. On an HTML page (where selectors apply) one it rejects (any
member of a group) returns, with a nil error, only
`<error>Unsupported or invalid selector "<sel>": <reason>.</error>` or
`<error>Unsupported or invalid exclude selector "<sel>": <reason>.</error>`,
no page content (`<sel>` and `<reason>` with control characters as spaces,
`<` `>` as `‹` `›`, cut to 200 runes).
With all of them unset (the default),
output is byte-identical to upstream.

### Body cap: `MaxBodyBytes` (10 MiB by default)

`Options.MaxBodyBytes` caps the size of the response body. A body larger than
the cap fails the fetch with a `Failed to fetch <url>: response body exceeds N
bytes` error before any decoding, so a model-chosen (or prompt-injected) URL to
a huge file cannot exhaust memory. `0` (the default) applies
`webfetch.DefaultMaxBodyBytes` (10 MiB); a negative value disables the cap,
which is upstream's unbounded behaviour. This is the only default that differs
from upstream, and it only matters for bodies over the cap: anything smaller is
processed identically.

## Fidelity

The observable contract of the upstream tool is reproduced closely: the
autonomous `User-Agent`, the HTML/raw content-type heuristic, the
`Contents of <url>:` wrapper, and the truncation / error strings.

Content extraction is the one place byte-for-byte parity is impossible: upstream
runs Mozilla Readability.js in a Node subprocess (`readabilipy`) plus
`markdownify`. This module uses a trimmed copy of
[`codeberg.org/readeck/go-readability`](https://codeberg.org/readeck/go-readability)
v2.1.3 (a maintained Go port of the same Readability.js, vendored as
`internal/readability` to drop its weakly maintained transitive deps) and
[`html-to-markdown`](https://github.com/JohannesKaufmann/html-to-markdown)
configured to match markdownify's defaults (ATX headings, `*` bullets, `*`
emphasis). On typical pages the output is byte-identical; the only observed
difference is readability's URL normalization (e.g. a trailing slash added to
bare links).

## SSRF protection: only public IPs are reachable

By default the fetcher can reach **only globally-routable public unicast
addresses**. This directly addresses the standard fetch-server caveat that such
a tool "can access local/internal IP addresses and may represent a security
risk": here it cannot.

Enforcement is in the dialer (`net.Dialer.Control`), so it runs **after DNS
resolution** against the concrete IP the socket will connect to — which also
covers HTTP redirects (every hop is re-dialed and re-checked) and DNS-rebinding
(the resolved IP is what's validated, not the hostname). It is a strict
default-deny allowlist; the following are refused:

- loopback (`127.0.0.0/8`, `::1`) and unspecified (`0.0.0.0`, `::`)
- private (`10/8`, `172.16/12`, `192.168/16`) and IPv6 ULA (`fc00::/7`)
- carrier-grade NAT (`100.64.0.0/10`)
- link-local, including the cloud metadata endpoint `169.254.169.254`
- broadcast, multicast, and IANA special-use / documentation / benchmarking
  ranges
- IPv4-mapped IPv6 forms of any of the above (normalized before checking)

Code that makes its own requests can reuse the guard: set
`webfetch.GuardedControl` as the `Control` of the `net.Dialer` its transport
dials through. `webfetch.IsPublicIP` answers the same question for a single
IP; use it only on an IP, never to vet a hostname before dialing it (DNS
rebinding). The rejected set may grow in any release.
