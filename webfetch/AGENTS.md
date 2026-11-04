# AGENTS.md

## What this is

`webfetch` (`github.com/trick77/toolkit/webfetch`; was github.com/trick77/webfetch until v0.4.11, archived
after the move) is a dependency-light Go **library** — one public package, no CLI and no server binary. It ports the
Python `mcp-server-fetch`. Callers use it in-process:
`webfetch.Fetch(ctx, url, webfetch.Options{...})`.

Staying fully **in-process** — no subprocess, no sidecar container — is the whole
point of the port. Keep it that way: any new dependency must be pure-Go, and keep the
dependency set minimal.

webfetch guards the download (SSRF, size caps), not every parser threat. PDFs
are not parsed here: `PDFHandler` hands the bytes to the caller, who isolates
the parser. Don't add an in-process PDF parser back (a bomb can't be bounded,
#30).

`internal/readability` is a trimmed copy of go-readability v2.1.3 (deltas in its
package doc), vendored to shed dateparse/chardet/go-shiori deps. Don't rewrite
the extraction algorithm; port upstream fixes by hand. `testdata/readability`
goldens were made by the original library: a diff there is a regression, not a
golden to regenerate.

## Hard constraint — upstream fidelity

This library deliberately reproduces the *observable contract* of upstream
`mcp-server-fetch`. Do NOT rephrase, reformat, or "improve":

- the exact upstream strings: the upstream `<error>…</error>` messages (not
  `errNoSelector`, which is ours), the `Failed to fetch` prefix, the
  `Contents of %s:` prefix, and the "Content type … cannot be simplified" note
- the autonomous `User-Agent` (`DefaultUserAgentAutonomous`)
- the html-to-markdown config (`atx` headings, `*` bullets, `*` emphasis) — this
  matches Python markdownify defaults; changing it breaks byte-parity
- rune-based (not byte-based) `start_index` / `max_length` slicing, which matches
  Python `str` slicing

**Extending is fine; diverging by default is not.** Capabilities beyond upstream
are added as opt-in `Options` fields that default to off, so a zero-value
`Options` produces byte-identical output. Follow that pattern for anything new.
Existing extensions: `IncludeMetadata`, `PDFHandler` (+ `MaxPDFBytes`), and the `FullPage` /
`Selector` / `ExcludeSelectors` escape hatch.

The one deliberate exception is `MaxBodyBytes`: it defaults to a 10 MiB cap
(`DefaultMaxBodyBytes`), with a negative value meaning unlimited, because an
in-process fetcher must not let a model-chosen URL exhaust memory. Only bodies
over the cap behave differently from upstream. Do not "fix" the default back to
unlimited.

## Hard constraint — SSRF guard (`ssrf.go`)

Strict default-deny: only globally-routable public unicast IPs may be reached.
Enforcement lives in `net.Dialer.Control`, which runs *after DNS resolution* so
it also covers redirects and DNS-rebinding. Do not weaken it, do not move the
check to the hostname level, and do not add exceptions. Apply any change
consistently across IPv4 and IPv6, including IPv4-mapped IPv6 forms. `dialControl`
is a test-only hook so in-package tests can reach a loopback server — don't
repurpose or export it.

## Git & CI

The repo's root AGENTS.md (gate, release, commit style). Subjects here: `webfetch: …` (or `ssrf: …`,
`markdown: …`).
