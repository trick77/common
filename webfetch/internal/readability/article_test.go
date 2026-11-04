package readability

import (
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // TestParseTimeHostZone loads zones on hosts without zoneinfo
)

// Expected values are what dateparse.ParseAny (the parser this replaced)
// returned for the same input on a UTC host, formatted RFC 3339; "" is a
// parse error.
var timeCases = []struct{ in, want string }{
	// Shapes seen in readability's test corpus.
	{"2014-09-02T08:35:27-04:00", "2014-09-02T08:35:27-04:00"},
	{"2015-07-24T05:36:09+01:00", "2015-07-24T05:36:09+01:00"},
	{"2017-11-03 02:22:36.000000", "2017-11-03T02:22:36Z"},
	{"2017-11-24T18:42:20.314667", "2017-11-24T18:42:20.314667Z"},
	{"2018-12-21T12:55:00Z", "2018-12-21T12:55:00Z"},
	{"2020-09-21", "2020-09-21T00:00:00Z"},
	{"2015-10-15T02:19:15.607Z", "2015-10-15T02:19:15.607Z"},
	{"2018-04-05T06:00", "2018-04-05T06:00:00Z"},
	{"2020-05-22T16:30:40.887-04:00", "2020-05-22T16:30:40.887-04:00"},
	{"2021-11-01T10:52:50+0100", "2021-11-01T10:52:50+01:00"},
	{"Fri Apr 01 2022 20:42:09 GMT+0000 (UTC)", "2022-04-01T20:42:09Z"},
	{"Fri Dec 27 2024 00:09:13 GMT+0100 (CET)", "2024-12-27T00:09:13+01:00"},
	// Common variants.
	{"2006-01-02T15:04:05", "2006-01-02T15:04:05Z"},
	{"2006-01-02 15:04:05", "2006-01-02T15:04:05Z"},
	{"2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05-07:00"},
	{"2006-01-02 15:04:05-07:00", "2006-01-02T15:04:05-07:00"},
	{"2006-01-02T15:04:05.999999999+05:30", "2006-01-02T15:04:05.999999999+05:30"},
	{"2006-01-02T15:04Z", "2006-01-02T15:04:00Z"},
	{"2006-01-02T15:04:05 +01:00", "2006-01-02T15:04:05+01:00"},
	{"2024-01-02T10:00:00+01", "2024-01-02T10:00:00+01:00"},
	{"2024-01-02T10:00:00.123-05", "2024-01-02T10:00:00.123-05:00"},
	{"2024-01-02T10:00+01", "2024-01-02T10:00:00+01:00"},
	{"2024-01-02 10:00:00+05", "2024-01-02T10:00:00+05:00"},
	{"2024-01-02T10:00:00 GMT", "2024-01-02T10:00:00Z"},
	{"2024-01-02T10:00:00 UTC", "2024-01-02T10:00:00Z"},
	{"2024-01-02 10:00:00 UTC", "2024-01-02T10:00:00Z"},
	{"2024-01-02 10:00:00 GMT", "2024-01-02T10:00:00Z"},
	{"2024-01-02 10:00:00 +0000 UTC", "2024-01-02T10:00:00Z"},
	{"2024-01-02 10:00:00 +0100 CET", "2024-01-02T10:00:00+01:00"},
	{"Mon, 02 Jan 2006 15:04:05 GMT", "2006-01-02T15:04:05Z"},
	{"Mon, 02 Jan 2006 15:04 GMT", "2006-01-02T15:04:00Z"},
	{"Mon, 02 Jan 2006 15:04 -0700", "2006-01-02T15:04:00-07:00"},
	{"Mon, 02 Jan 2006 15:04:05 -0700", "2006-01-02T15:04:05-07:00"},
	{"Mon, 2 Jan 2006 15:04:05 +0000", "2006-01-02T15:04:05Z"},
	{"Monday, 02-Jan-06 15:04:05 GMT", "2006-01-02T15:04:05Z"},
	{"Monday, 02-Jan-2006 15:04:05 GMT", "2006-01-02T15:04:05Z"},
	{"Mon Jan 2 15:04:05 2006", "2006-01-02T15:04:05Z"},
	{"Mon Jan 2 15:04:05 MST 2006", "2006-01-02T15:04:05Z"},
	{"Mon, 02 Jan 2006 15:04:05 PST", "2006-01-02T15:04:05Z"},
	{"January 2, 2006 3:04 PM", "2006-01-02T15:04:00Z"},
	{"June 5, 2024 10:30 am", "2024-06-05T10:30:00Z"},
	{"January 2, 2006 15:04", "2006-01-02T15:04:00Z"},
	{"Jan 2, 2006 3:04 PM", "2006-01-02T15:04:00Z"},
	{"Jan 2, 2006 3:04 pm", "2006-01-02T15:04:00Z"},
	{"Jan 2, 2006 15:04", "2006-01-02T15:04:00Z"},
	{"Tuesday, January 2, 2024", "2024-01-02T00:00:00Z"},
	{"January 2, 2006", "2006-01-02T00:00:00Z"},
	{"Jan 2, 2006", "2006-01-02T00:00:00Z"},
	{"2 January 2006", "2006-01-02T00:00:00Z"},
	{"02 Jan 2006", "2006-01-02T00:00:00Z"},
	{"2 Jan 2006", "2006-01-02T00:00:00Z"},
	{"2006/01/02", "2006-01-02T00:00:00Z"},
	{"2024/06/25 10:00:00", "2024-06-25T10:00:00Z"},
	{"2024/06/25 10:00", "2024-06-25T10:00:00Z"},
	{"Tue, 25 Jun 2024 10:00:00 +0000 (UTC)", "2024-06-25T10:00:00Z"},
	{"Tue, 25 Jun 2024 10:00:00 +0200 (CEST)", "2024-06-25T10:00:00+02:00"},
	{"Tuesday, 25-Jun-2024 10:00:00 GMT-11", "2024-06-25T10:00:00-11:00"},
	{"  2024-06-25  ", "2024-06-25T00:00:00Z"},
	{"2006-01", "2006-01-01T00:00:00Z"},
	{"20060102", "2006-01-02T00:00:00Z"},
	{"not a date", ""},
	{"13:39", ""},
	{"", ""},
	{"2006-01-02T15:04:05+00:00Z", ""},
	// A "(zone)" comment is cut only after GMT or a numeric offset, not after
	// any value.
	{"2024-06-25 (Tuesday)", ""},
	{"2024-06-25 10:00 (CEST)", ""},
	// dateparse rejected these GMT±h zones; they read with the offset they
	// name (time.Parse alone leaves the instant at UTC).
	{"Tue, 25 Jun 2024 10:00:00 GMT+2", "2024-06-25T10:00:00+02:00"},
	{"Tue Jun 25 10:00:00 GMT+2 2024", "2024-06-25T10:00:00+02:00"},
	{"2024-06-25 10:00:00 +0200 GMT+2", "2024-06-25T10:00:00+02:00"},
	// dateparse accepted these; dropped as rare or ambiguous (day/month
	// order), so they now read as no published time.
	{"02.01.2006", ""},
	{"01/02/2006", ""},
	{"2006.01.02", ""},
	{"2006-1-2", ""},
	{"2024-1-02", ""},
	{"2024", ""},
	{"Jan 2, 2006 at 3:04pm", ""},
}

// TestParseTimeHostZone pins that parseTime ignores time.Local: Go resolves a
// zone abbreviation against the local zone in time.Parse, so "MST" read as
// -07:00 on a Denver host and as +00:00 elsewhere.
func TestParseTimeHostZone(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	for _, name := range []string{"America/Denver", "America/Los_Angeles", "Europe/Zurich", "Asia/Kolkata"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		time.Local = loc
		for _, c := range timeCases {
			got := ""
			if tm, err := parseTime(c.in); err == nil {
				got = tm.Format(time.RFC3339Nano)
			}
			if got != c.want {
				t.Errorf("%s: parseTime(%q) = %q, want %q", name, c.in, got, c.want)
			}
		}
	}
}

func TestParseTime(t *testing.T) {
	for _, c := range timeCases {
		got := ""
		if tm, err := parseTime(c.in); err == nil {
			got = tm.Format(time.RFC3339Nano)
		}
		if got != c.want {
			t.Errorf("parseTime(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseTimeUnix(t *testing.T) {
	cases := map[string]string{
		"1700000000":          "2023-11-14T22:13:20Z",
		"1700000000123":       "2023-11-14T22:13:20.123Z",
		"1700000000123456":    "2023-11-14T22:13:20.123456Z",
		"1700000000123456789": "2023-11-14T22:13:20.123456789Z",
	}
	for in, want := range cases {
		got, err := parseTime(in)
		if err != nil || got.Format(time.RFC3339Nano) != want || got.Location() != time.UTC {
			t.Errorf("parseTime(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"170000000", "+1700000000", "-170000000"} {
		if _, err := parseTime(in); err == nil {
			t.Errorf("parseTime(%q): want error", in)
		}
	}
}

func TestArticlePublishedTime(t *testing.T) {
	if _, err := (Article{}).PublishedTime(); !errors.Is(err, ErrTimestampMissing) {
		t.Errorf("missing: err = %v, want ErrTimestampMissing", err)
	}
	if _, err := (Article{publishedTime: "yesterday"}).PublishedTime(); err == nil || !strings.Contains(err.Error(), "publishedTime") {
		t.Errorf("unparsable: err = %v", err)
	}
	got, err := (Article{publishedTime: "2025-03-14T09:26:53+0100"}).PublishedTime()
	if err != nil || got.Format(time.RFC3339) != "2025-03-14T09:26:53+01:00" {
		t.Errorf("valid: %v, %v", got, err)
	}
}
