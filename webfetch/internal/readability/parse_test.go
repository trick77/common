package readability

import (
	"strings"
	"testing"
)

func TestParseNormalizesText(t *testing.T) {
	// Decomposed e + U+0301 becomes U+00E9; a literal U+00AD is dropped.
	var (
		shy  = string(rune(0xAD))
		acut = string(rune(0x301))
		nfdE = "e" + acut
		nfcE = string(rune(0xE9))
	)
	page := "<html><head><title>Caf" + nfdE + " au lait</title></head><body><article><p>" +
		strings.Repeat("Le caf"+nfdE+" a rouvert apr"+nfdE+"s des mois de tra"+shy+"vaux. ", 20) +
		"</p></article></body></html>"
	a, err := FromReader(strings.NewReader(page), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Caf" + nfcE + " au lait"; a.Title() != want {
		t.Errorf("title = %q, want %q", a.Title(), want)
	}
	var b strings.Builder
	if err := a.RenderHTML(&b); err != nil {
		t.Fatal(err)
	}
	if html := b.String(); strings.Contains(html, shy) || strings.Contains(html, acut) || !strings.Contains(html, "travaux") {
		t.Errorf("content not normalized: %q", html[:min(len(html), 120)])
	}
	if err := (Article{}).RenderHTML(&b); err == nil {
		t.Error("RenderHTML on nil Node: want error")
	}
}
