package re2go

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// The generated matchers must agree with the regular expressions they were
// generated from (the "Original pattern" in each .re file). Inputs are every
// keyword, every keyword prefix (the DFA's partial-match and fallback
// states), pairs of them, and seeded random strings over the keywords'
// alphabet, each also upper-cased.
var matchers = []struct {
	name string
	fn   func(string) bool
	re   *regexp.Regexp
}{
	{"IsByline", IsByline, regexp.MustCompile(`(?i)byline|author|dateline|writtenby|p-author`)},
	{"IsUnlikelyCandidates", IsUnlikelyCandidates, regexp.MustCompile(`(?i)-ad-|ai2html|banner|breadcrumbs|combx|comment|community|cover-wrap|disqus|extra|footer|gdpr|header|legends|menu|related|remark|replies|rss|shoutbox|sidebar|skyscraper|social|sponsor|supplemental|ad-break|agegate|pagination|pager|popup|yom-remote`)},
	{"MaybeItsACandidate", MaybeItsACandidate, regexp.MustCompile(`(?i)and|article|body|column|content|main|mathjax|shadow`)},
	{"IsPositiveClass", IsPositiveClass, regexp.MustCompile(`(?i)article|body|content|entry|hentry|h-entry|main|page|pagination|post|text|blog|story`)},
	{"IsNegativeClass", IsNegativeClass, regexp.MustCompile(`(?i)(^| )(hid|hidden|d-none)( |$)|-ad-|banner|combx|comment|com-|contact|footer|gdpr|masthead|meta|outbrain|promo|related|share|shoutbox|sidebar|skyscraper|sponsor|shopping|tags|widget`)},
}

// keywordsOf splits a pattern's top-level alternation into literal words.
func keywordsOf(re *regexp.Regexp) []string {
	s := strings.TrimPrefix(re.String(), "(?i)")
	s = strings.NewReplacer("(^| )(", "", ")( |$)", "", "(", "", ")", "").Replace(s)
	return strings.Split(s, "|")
}

func matcherInputs(keywords []string, alphabet string) []string {
	var in []string
	var prefixes []string
	for _, k := range keywords {
		for i := 1; i <= len(k); i++ {
			prefixes = append(prefixes, k[:i])
		}
	}
	in = append(in, "", " ", "-", "x")
	for _, p := range prefixes {
		in = append(in, p, " "+p, p+" ", " "+p+" ", "x"+p, p+"x", p+"-")
	}
	for _, a := range keywords {
		for _, b := range prefixes {
			in = append(in, a+b, b+a, a+" "+b)
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		var b strings.Builder
		for range 1 + r.IntN(14) {
			b.WriteByte(alphabet[r.IntN(len(alphabet))])
		}
		in = append(in, b.String())
	}
	return in
}

func TestMatchersAgreeWithRegexp(t *testing.T) {
	for _, m := range matchers {
		kw := keywordsOf(m.re)
		alphabet := " -" + strings.Join(kw, "")
		fails := 0
		for _, s := range matcherInputs(kw, alphabet) {
			for _, in := range []string{s, strings.ToUpper(s)} {
				if got, want := m.fn(in), m.re.MatchString(in); got != want {
					t.Errorf("%s(%q) = %v, regexp %v", m.name, in, got, want)
					if fails++; fails > 10 {
						t.FailNow()
					}
				}
			}
		}
	}
}

func TestNormalizeSpacesAgreesWithRegexp(t *testing.T) {
	re := regexp.MustCompile(`\s{2,}`)
	r := rand.New(rand.NewPCG(3, 4))
	const alphabet = "ab  \t\n\r\f"
	for range 5000 {
		var b strings.Builder
		for range r.IntN(16) {
			b.WriteByte(alphabet[r.IntN(len(alphabet))])
		}
		in := b.String()
		if got, want := NormalizeSpaces(in), re.ReplaceAllString(in, " "); got != want {
			t.Fatalf("NormalizeSpaces(%q) = %q, regexp %q", in, got, want)
		}
	}
}
