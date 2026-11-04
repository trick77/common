// Package readability is a Go package that find the main readable
// content from a HTML page. It works by removing clutter like buttons,
// ads, background images, script, etc.
//
// This package is based from Readability.js by Mozilla, and written line
// by line to make sure it looks and works as similar as possible. This
// way, hopefully all web page that can be parsed by Readability.js
// are parse-able by go-readability as well.
//
// It is a copy of codeberg.org/readeck/go-readability/v2 v2.1.3 (MIT, see
// LICENSE), trimmed to what webfetch uses. The extraction algorithm
// (parser.go, re2go/) is unchanged. Differences from upstream:
//   - Parse no longer guesses the charset (gogs/chardet): its input must be
//     UTF-8, which webfetch's decodeBody guarantees. The NFC normalization
//     and soft-hyphen removal upstream applied while parsing are kept, and
//     exported as NormalizeText for callers that build the tree themselves.
//   - PublishedTime parses with a fixed set of time layouts instead of
//     itlightning/dateparse; see parseTime.
//   - The go-shiori/dom helpers live in ./dom.
//   - Dropped: FromURL, FromDocument, ParseDocument, CheckDocument, the
//     render package (RenderText, Excerpt), ImageURL, Favicon, ModifiedTime.
//
// re2go/*.go are generated from re2go/*.re with
// `re2go -W -F --input-encoding utf8 --utf8 --no-generation-date -i X.re -o X.go`
// followed by gofmt.
package readability

import (
	"io"
	nurl "net/url"
)

// FromReader parses an `io.Reader` and returns the readable content. It's the wrapper
// or `Parser.Parse()` and useful if you only want to use the default parser.
func FromReader(input io.Reader, pageURL *nurl.URL) (Article, error) {
	parser := NewParser()
	return parser.Parse(input, pageURL)
}

// NormalizeText applies the text normalization Parse runs before parsing
// (NFC, soft hyphens removed), for callers that build the tree themselves,
// e.g. to hand it to ParseAndMutate.
func NormalizeText(r io.Reader) io.Reader {
	return normalizeTextEncoding(r)
}
