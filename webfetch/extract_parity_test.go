package webfetch

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractParity pins extraction output (frontmatter title / author /
// published / site / language, plus the Markdown body) on fixed pages. The
// goldens were produced by codeberg.org/readeck/go-readability v2.1.3 before
// it was vendored as internal/readability: a diff is a behaviour change, not a
// golden to regenerate. Fixtures are from that library's test corpus, plus two
// pages for soft hyphens / NFD text and JSON-LD / RFC 1123 dates.
func TestExtractParity(t *testing.T) {
	pages, err := filepath.Glob("testdata/readability/*.html")
	if err != nil || len(pages) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	base, _ := url.Parse("http://fakehost/test/page.html")
	for _, page := range pages {
		name := strings.TrimSuffix(filepath.Base(page), ".html")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(page)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(strings.TrimSuffix(page, ".html") + ".golden.md")
			if err != nil {
				t.Fatal(err)
			}
			body, err := decodeBody(raw, "text/html")
			if err != nil {
				t.Fatal(err)
			}
			if got := extractContentFromHTML(body, base, Options{IncludeMetadata: true}); got != string(want) {
				t.Errorf("output differs from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			// ExcludeSelectors builds the tree itself before Readability; with
			// nothing to exclude it must match the default path (NFC, soft
			// hyphens stripped).
			excl := Options{IncludeMetadata: true, ExcludeSelectors: []string{"#webfetch-no-such-element"}}
			if got := extractContentFromHTML(body, base, excl); got != string(want) {
				t.Errorf("ExcludeSelectors output differs from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			// Blank selectors are unset: the default path, metadata included.
			blank := Options{IncludeMetadata: true, Selector: " \t\n", ExcludeSelectors: []string{" "}}
			if got := extractContentFromHTML(body, base, blank); got != string(want) {
				t.Errorf("blank Selector output differs from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}
