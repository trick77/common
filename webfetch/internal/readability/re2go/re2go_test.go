package re2go

import (
	"strings"
	"testing"
)

func Test_IsByline(t *testing.T) {
	tests := map[string]bool{
		"article-byline":   true,
		"author":           true,
		"dateline":         true,
		"meta-prep-author": true,
		"writtenbynames":   true,

		"article-line": false,
		"autor":        false, //nolint:misspell // deliberate near-miss
		"bynames":      false,
		"date":         false,
		"meta-autor":   false, //nolint:misspell // deliberate near-miss
	}
	for matchString, expected := range tests {
		if got := IsByline(matchString); got != expected {
			t.Errorf("IsByline(%q): expected %v, got %v", matchString, expected, got)
		}
		upper := strings.ToUpper(matchString)
		if got := IsByline(upper); got != expected {
			t.Errorf("IsByline(%q): expected %v, got %v", upper, expected, got)
		}
	}
}

func Test_IsPositiveClass(t *testing.T) {
	tests := map[string]bool{
		"article":    true,
		"blog":       true,
		"body":       true,
		"content":    true,
		"entry":      true,
		"h-entry":    true,
		"hentry":     true,
		"main":       true,
		"page":       true,
		"pagination": true,
		"post":       true,
		"story":      true,
		"text":       true,

		"container":   false,
		"description": false,
		"footer":      false,
		"gallery":     false,
		"header":      false,
		"layout":      false,
		"navigation":  false,
		"news":        false,
		"sidebar":     false,
		"toolbar":     false,
		"widget":      false,
	}
	for matchString, expected := range tests {
		if got := IsPositiveClass(matchString); got != expected {
			t.Errorf("IsPositiveClass(%q): expected %v, got %v", matchString, expected, got)
		}
		upper := strings.ToUpper(matchString)
		if got := IsPositiveClass(upper); got != expected {
			t.Errorf("IsPositiveClass(%q): expected %v, got %v", upper, expected, got)
		}
	}
}

func Test_IsNegativeClass(t *testing.T) {
	tests := map[string]bool{
		"-ad-":           true,
		"ad-banner":      true,
		"banner":         true,
		"class hid good": true,
		"class hid":      true,
		"com-":           true,
		"combx":          true,
		"comment":        true,
		"contact":        true,
		"d-none":         true,
		"footer":         true,
		"gdpr":           true,
		"hid class":      true,
		"hid":            true,
		"hidden":         true,
		"masthead":       true,
		"meta":           true,
		"outbrain":       true,
		"promo":          true,
		"related":        true,
		"share":          true,
		"shopping":       true,
		"shoutbox":       true,
		"sidebar":        true,
		"skyscraper":     true,
		"sponsor":        true,
		"tags":           true,
		"widget":         true,

		"catalog":         false,
		"details":         false,
		"foot":            false,
		"footnote":        false,
		"gallery":         false,
		"media":           false,
		"navbar":          false,
		"news-feed":       false,
		"overview":        false,
		"profile":         false,
		"scroll":          false,
		"sad-nonet":       false,
		"support":         false,
		"tool":            false,
		"toolbar":         false,
		"user-menu":       false,
		"visually-hidden": false,
	}
	for matchString, expected := range tests {
		if got := IsNegativeClass(matchString); got != expected {
			t.Errorf("IsNegativeClass(%q): expected %v, got %v", matchString, expected, got)
		}
		upper := strings.ToUpper(matchString)
		if got := IsNegativeClass(upper); got != expected {
			t.Errorf("IsNegativeClass(%q): expected %v, got %v", upper, expected, got)
		}
	}
}

func Test_IsUnlikelyCandidates(t *testing.T) {
	tests := map[string]bool{
		"-ad-":         true,
		"ad-banner":    true,
		"ad-break":     true,
		"agegate":      true,
		"ai2html":      true,
		"banner":       true,
		"breadcrumbs":  true,
		"combx":        true,
		"comment":      true,
		"community":    true,
		"cover-wrap":   true,
		"disqus":       true,
		"extra":        true,
		"footer":       true,
		"gdpr":         true,
		"header":       true,
		"legends":      true,
		"menu":         true,
		"pager":        true,
		"pagination":   true,
		"popup":        true,
		"related":      true,
		"remark":       true,
		"replies":      true,
		"rss":          true,
		"shoutbox":     true,
		"sidebar":      true,
		"skyscraper":   true,
		"social":       true,
		"sponsor":      true,
		"supplemental": true,
		"yom-remote":   true,

		"catalog":   false,
		"container": false,
		"gallery":   false,
		"newsfeed":  false,
		"overview":  false,
		"summary":   false,
		"toolbar":   false,
	}
	for matchString, expected := range tests {
		if got := IsUnlikelyCandidates(matchString); got != expected {
			t.Errorf("IsUnlikelyCandidates(%q): expected %v, got %v", matchString, expected, got)
		}
		upper := strings.ToUpper(matchString)
		if got := IsUnlikelyCandidates(upper); got != expected {
			t.Errorf("IsUnlikelyCandidates(%q): expected %v, got %v", upper, expected, got)
		}
	}
}

func Test_MaybeItsACandidate(t *testing.T) {
	tests := map[string]bool{
		"and":     true,
		"article": true,
		"body":    true,
		"column":  true,
		"content": true,
		"main":    true,
		"shadow":  true,

		"footer":  false,
		"gallery": false,
		"header":  false,
		"menu":    false,
		"navbar":  false,
		"text":    false,
	}
	for matchString, expected := range tests {
		if got := MaybeItsACandidate(matchString); got != expected {
			t.Errorf("MaybeItsACandidate(%q): expected %v, got %v", matchString, expected, got)
		}
		upper := strings.ToUpper(matchString)
		if got := MaybeItsACandidate(upper); got != expected {
			t.Errorf("MaybeItsACandidate(%q): expected %v, got %v", upper, expected, got)
		}
	}
}

func Test_CSSUtilityClasses(t *testing.T) {
	// Utility classnames, like for example those found in Tailwind projects, that should generally
	// never trigger any content scoring heuristics.
	tests := []string{
		"overflow-hidden",
		"sm:hidden",
		"stacked-navigation--hidden",
		"py-3 details-overlay-dark",
		"overflow-x-scroll",
		// "text-ellipsis md:text-clip",
		// "before:content-[attr(before)]",
	}
	for _, matchString := range tests {
		if IsPositiveClass(matchString) {
			t.Errorf("IsPositiveClass() triggered by CSS utility class %q", matchString)
		}
		if IsNegativeClass(matchString) {
			t.Errorf("IsNegativeClass() triggered by CSS utility class %q", matchString)
		}
		if MaybeItsACandidate(matchString) {
			t.Errorf("MaybeItsACandidate() triggered by CSS utility class %q", matchString)
		}
		if IsUnlikelyCandidates(matchString) {
			t.Errorf("IsUnlikelyCandidates() triggered by CSS utility class %q", matchString)
		}
	}
}

func Test_NormalizeSpaces(t *testing.T) {
	tests := map[string]string{
		"some   sentence":               "some sentence",
		"with \t \ttabs":                "with tabs",
		" single space is ok ":          " single space is ok ",
		"   multi   space   removed   ": " multi space removed ",
	}
	for in, want := range tests {
		if got := NormalizeSpaces(in); got != want {
			t.Errorf("NormalizeSpaces(%q) = %q, want %q", in, got, want)
		}
	}
}
