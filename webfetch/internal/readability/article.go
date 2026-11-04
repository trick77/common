package readability

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ErrTimestampMissing is returned by PublishedTime when the page has no
// published-time metadata.
var ErrTimestampMissing = errors.New("timestamp not found in document")

// Article is the final readable content of a parsed article.
type Article struct {
	// Node is the top-level container of cleaned-up article content. It may be nil if there were
	// errors or if article content was blank.
	Node *html.Node

	title         string
	byline        string
	siteName      string
	language      string
	publishedTime string
}

// RenderHTML writes the article content (Node) as HTML.
func (a Article) RenderHTML(w io.Writer) error {
	if a.Node == nil {
		return fmt.Errorf("the Node field is nil")
	}
	return html.Render(w, a.Node)
}

// Title is the article title.
func (a Article) Title() string {
	return a.title
}

// Byline is the article author.
func (a Article) Byline() string {
	return a.byline
}

// SiteName is the name of the publishing site.
func (a Article) SiteName() string {
	return a.siteName
}

// Language is the document language (the html lang attribute).
func (a Article) Language() string {
	return a.language
}

// PublishedTime is the time when the article was published. If no timestamp was found in the
// article metadata, the error will be ErrTimestampMissing.
func (a Article) PublishedTime() (time.Time, error) {
	if a.publishedTime == "" {
		return time.Time{}, ErrTimestampMissing
	}
	t, err := parseTime(a.publishedTime)
	if err != nil {
		return t, fmt.Errorf("error parsing publishedTime: %w", err)
	}
	return t, nil
}

// timeLayouts are the published-time shapes found in meta tags and JSON-LD:
// ISO 8601 / RFC 3339 and their common relaxations, the RFC 1123 / 850 / 822
// family, C, Go and JavaScript Date strings, and spelled-out dates. time.Parse
// accepts fractional seconds after the seconds field even when a layout has
// none; Z0700 accepts "Z" and "+0100", Z07 "Z" and "+01". A single-digit day
// or hour layout also takes two digits. Zone-less values are UTC.
var timeLayouts = []string{
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05Z07",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05 Z07:00",
	"2006-01-02T15:04:05 Z0700",
	"2006-01-02T15:04:05 UTC",
	"2006-01-02T15:04:05 GMT",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04Z0700",
	"2006-01-02T15:04Z07",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05Z0700",
	"2006-01-02 15:04:05Z07",
	"2006-01-02 15:04:05 Z07:00",
	"2006-01-02 15:04:05 Z0700",
	"2006-01-02 15:04:05 -0700 MST", // Go time.Time.String
	"2006-01-02 15:04:05 UTC",
	"2006-01-02 15:04:05 GMT",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02 15:04",
	"2006/01/02",
	"2006-01",
	"20060102",
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04 -0700",
	"Mon, 2 Jan 2006 15:04 MST",
	time.RFC850,
	"Monday, 02-Jan-2006 15:04:05 MST", // RFC 850 with a four-digit year
	time.RFC822Z,
	time.RFC822,
	time.ANSIC,
	time.UnixDate,
	"Mon Jan 02 2006 15:04:05 GMT-0700", // JavaScript Date.toString, "(zone name)" cut off
	"January 2, 2006 3:04 PM",
	"January 2, 2006 3:04 pm",
	"January 2, 2006 15:04",
	"Jan 2, 2006 3:04 PM",
	"Jan 2, 2006 3:04 pm",
	"Jan 2, 2006 15:04",
	"Monday, January 2, 2006",
	"January 2, 2006",
	"Jan 2, 2006",
	"2 January 2006",
	"2 Jan 2006",
}

// parseTime parses a metadata timestamp with timeLayouts, or as Unix seconds,
// milliseconds, microseconds or nanoseconds (10, 13, 16 or 19 digits). It
// replaces dateparse.ParseAny, which accepted many more (and ambiguous)
// shapes; a value matching no layout is an error, as an unparsable one was
// before. Results never depend on the host's zone: Unix values are UTC, and a
// zone abbreviation without a numeric offset (other than UTC/GMT) reads as
// offset zero instead of resolving against time.Local; a "GMT+2" zone reads
// as +02:00. A trailing " (zone)" comment is cut after "GMT" or a numeric
// offset.
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " ("); i > 0 && strings.HasSuffix(s, ")") && (strings.Contains(s[:i], "GMT") || endsWithOffset(s[:i])) {
		s = s[:i]
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && s[0] != '-' && s[0] != '+' {
		switch len(s) {
		case 10:
			return time.Unix(n, 0).UTC(), nil
		case 13:
			return time.UnixMilli(n).UTC(), nil
		case 16:
			return time.UnixMicro(n).UTC(), nil
		case 19:
			return time.Unix(0, n).UTC(), nil
		}
	}
	for _, layout := range timeLayouts {
		t, err := time.ParseInLocation(layout, s, time.UTC)
		if err != nil {
			continue
		}
		if name, _ := t.Zone(); len(name) > 3 && strings.HasPrefix(name, "GMT") && !strings.Contains(layout, "-0700") {
			// time.Parse gives "GMT+2" its offset but leaves the instant at
			// UTC; re-anchor the wall clock in that zone.
			u := t.UTC()
			t = time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), u.Second(), u.Nanosecond(), t.Location())
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format %q", s)
}

// endsWithOffset reports whether s ends in a numeric zone offset (" +0200").
func endsWithOffset(s string) bool {
	n := len(s)
	if n < 6 || s[n-6] != ' ' || (s[n-5] != '+' && s[n-5] != '-') {
		return false
	}
	for _, c := range s[n-4:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
