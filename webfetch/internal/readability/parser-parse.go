package readability

import (
	"fmt"
	"io"
	nurl "net/url"

	"github.com/trick77/toolkit/webfetch/internal/readability/dom"
	"golang.org/x/net/html"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Parse parses a reader and find the main readable content. The input must
// be UTF-8.
func (ps *Parser) Parse(input io.Reader, pageURL *nurl.URL) (Article, error) {
	// Parse input
	doc, err := html.Parse(normalizeTextEncoding(input))
	if err != nil {
		return Article{}, fmt.Errorf("failed to parse input: %w", err)
	}

	return ps.ParseAndMutate(doc, pageURL)
}

// normalizeTextEncoding convert text encoding from NFD to NFC.
// It also remove soft hyphen since apparently it's useless in web.
// See: https://web.archive.org/web/19990117011731/http://www.hut.fi/~jkorpela/shy.html
// (From go-shiori/dom's Parse, which ran it after its charset guess.)
func normalizeTextEncoding(r io.Reader) io.Reader {
	fnSoftHyphen := func(r rune) bool { return r == 0xAD } // U+00AD SOFT HYPHEN
	softHyphenSet := runes.Predicate(fnSoftHyphen)
	transformer := transform.Chain(norm.NFD, runes.Remove(softHyphenSet), norm.NFC)
	return transform.NewReader(r, transformer)
}

// ParseAndMutate parses doc and finds the main readable content, mutating
// doc during parsing.
func (ps *Parser) ParseAndMutate(doc *html.Node, pageURL *nurl.URL) (Article, error) {
	ps.doc = doc

	// Reset parser data
	ps.articleTitle = ""
	ps.articleByline = ""
	ps.articleDir = ""
	ps.articleSiteName = ""
	ps.documentURI = pageURL
	ps.attempts = []parseAttempt{}
	// These flags could get modified during subsequent passes in grabArticle
	ps.flags = flags{
		stripUnlikelys:     true,
		useWeightClasses:   true,
		cleanConditionally: true,
	}

	// Avoid parsing too large documents, as per configuration option
	if ps.MaxElemsToParse > 0 {
		numTags := len(dom.GetElementsByTagName(ps.doc, "*"))
		if numTags > ps.MaxElemsToParse {
			return Article{}, fmt.Errorf("documents too large: %d elements", numTags)
		}
	}

	// Unwrap image from noscript
	ps.unwrapNoscriptImages(ps.doc)

	// Extract JSON-LD metadata before removing scripts
	var jsonLd map[string]string
	if !ps.DisableJSONLD {
		jsonLd = ps.getJSONLD()
	}

	// Remove script tags from the document.
	ps.removeScripts(ps.doc)

	// Prepares the HTML document
	ps.prepDocument()

	// Fetch metadata
	metadata := ps.getArticleMetadata(jsonLd)
	ps.articleTitle = metadata["title"]
	ps.articleByline = metadata["byline"]

	// Try to grab article content
	articleContent := ps.grabArticle()
	var readableNode *html.Node

	if articleContent != nil {
		ps.postProcessContent(articleContent)
		readableNode = dom.FirstElementChild(articleContent)
	}

	return Article{
		title:         ps.articleTitle,
		byline:        ps.articleByline,
		Node:          readableNode,
		siteName:      metadata["siteName"],
		language:      ps.articleLang,
		publishedTime: metadata["publishedTime"],
	}, nil
}
