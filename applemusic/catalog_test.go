package applemusic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTokens hands out "tok1", then "tok2" after an Invalidate.
type fakeTokens struct {
	invalidated atomic.Int32
	err         error
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return fmt.Sprintf("tok%d", f.invalidated.Load()+1), nil
}

func (f *fakeTokens) Invalidate(string) { f.invalidated.Add(1) }

func song(id, album, date string, preview bool) string {
	prev := `[]`
	if preview {
		prev = `[{"url":"https://audio.example/` + id + `.m4a"}]`
	}
	return fmt.Sprintf(`{"id":%q,"type":"songs","attributes":{"name":"Dancing Queen","artistName":"ABBA","albumName":%q,`+
		`"releaseDate":%q,"isrc":"SEA017600030","artwork":{"url":"https://img.example/%s/{w}x{h}bb.jpg"},"previews":%s}}`,
		id, album, date, id, prev)
}

// newCatalog is a client with reworm's import policy: four tries, no wait on a 429.
func newCatalog(t *testing.T, h http.Handler) (*Client, *fakeTokens) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok := &fakeTokens{}
	return &Client{BaseURL: srv.URL, HTTP: srv.Client(), Dev: tok, Tries: 4,
		Sleep: func(context.Context, time.Duration) error { return nil }}, tok
}

func TestSongsByISRC_followsNextAndSendsThePlayersHeaders(t *testing.T) {
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Origin") != "https://music.apple.com" ||
			r.Header.Get("Authorization") != "Bearer tok1" || r.Header.Get("Media-User-Token") != "" {
			t.Errorf("request %s %v", r.Method, r.Header)
		}
		if r.URL.Path != "/v1/catalog/ch/songs" || r.URL.Query().Get("filter[isrc]") != "SEA017600030,SEAYD7601020" {
			t.Errorf("url %s", r.URL)
		}
		if r.URL.Query().Get("offset") == "" {
			_, _ = fmt.Fprintf(w, `{"data":[%s],"next":"/v1/catalog/ch/songs?filter%%5Bisrc%%5D=SEA017600030,SEAYD7601020&offset=25"}`,
				song("1", "Arrival", "1976-10-11", true))
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s]}`, song("2", "Gold", "1992-09-21", false))
	}))
	got, err := c.SongsByISRC(context.Background(), "ch", []string{"SEA017600030", "SEAYD7601020"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Album != "Arrival" || !got[0].Preview || got[1].Preview {
		t.Fatalf("got %+v", got)
	}
	if got[0].Artwork != "https://img.example/1/{w}x{h}bb.jpg" || got[0].Year() != 1976 || got[0].ISRC != "SEA017600030" {
		t.Errorf("song %+v", got[0])
	}
	if none, err := c.SongsByISRC(context.Background(), "ch", nil); none != nil || err != nil {
		t.Errorf("no isrcs: %v, %v", none, err)
	}
}

func TestSongsByISRC_notFoundIsEmptyAndForeignNextFails(t *testing.T) {
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/catalog/us/songs" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[],"next":"https://evil.example/v1/x"}`)
	}))
	if got, err := c.SongsByISRC(context.Background(), "us", []string{"X"}); err != nil || len(got) != 0 {
		t.Errorf("404: %v, %v", got, err)
	}
	if _, err := c.SongsByISRC(context.Background(), "de", []string{"X"}); err == nil || !strings.Contains(err.Error(), "next link") {
		t.Errorf("foreign next: %v", err)
	}
}

func TestSearchSongs(t *testing.T) {
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1/catalog/de/search" || q.Get("term") != "ABBA Dancing Queen" || q.Get("types") != "songs" || q.Get("limit") != "10" {
			t.Errorf("url %s", r.URL)
		}
		_, _ = fmt.Fprintf(w, `{"results":{"songs":{"data":[%s]}}}`, strings.Replace(song("3", "Arrival", "1976", true), "https://img", "http://img", 1))
	}))
	got, err := c.SearchSongs(context.Background(), "de", "ABBA Dancing Queen", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "3" || got[0].Artwork != "" || got[0].Year() != 1976 {
		t.Errorf("got %+v (plain-http artwork must be dropped)", got)
	}
	if (Song{}).Year() != 0 {
		t.Error("year of an empty date")
	}
}

func TestSearchAlbumsAndAlbumTracks(t *testing.T) {
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/catalog/ch/search":
			if r.URL.Query().Get("types") != "albums" || r.URL.Query().Get("term") != "ABBA Arrival" {
				t.Errorf("url %s", r.URL)
			}
			_, _ = fmt.Fprint(w, `{"results":{"albums":{"data":[{"id":"1440820126","attributes":{"name":"Arrival (Bonus Track Version)",`+
				`"artistName":"ABBA","releaseDate":"1976-10-11"}}]}}}`)
		case "/v1/catalog/ch/albums/1440820126/tracks":
			_, _ = fmt.Fprintf(w, `{"data":[%s,{"id":"9","type":"music-videos","attributes":{"name":"Video"}}]}`,
				strings.Replace(song("1440820267", "Arrival (Bonus Track Version)", "1976-10-11", true), `"artistName"`, `"albumArtistName":"ABBA","artistName"`, 1))
		default:
			http.NotFound(w, r)
		}
	}))
	albums, err := c.SearchAlbums(context.Background(), "ch", "ABBA Arrival", 10)
	if err != nil || len(albums) != 1 || albums[0].ID != "1440820126" || albums[0].Year() != 1976 || albums[0].Artist != "ABBA" {
		t.Fatalf("albums %+v, %v", albums, err)
	}
	tracks, err := c.AlbumTracks(context.Background(), "ch", "1440820126")
	if err != nil || len(tracks) != 1 || tracks[0].ID != "1440820267" || tracks[0].AlbumArtist != "ABBA" {
		t.Fatalf("tracks %+v, %v", tracks, err)
	}
	if got, err := c.SearchAlbums(context.Background(), "xx", "x", 1); err != nil || len(got) != 0 {
		t.Errorf("404 search: %v, %v", got, err)
	}
}

func TestSongByID(t *testing.T) {
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/catalog/de/songs/1440820267" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s]}`, song("1440820267", "Arrival", "1976-10-11", true))
	}))
	s, ok, err := c.SongByID(context.Background(), "de", "1440820267")
	if err != nil || !ok || s.Album != "Arrival" {
		t.Errorf("%+v, %v, %v", s, ok, err)
	}
	if _, ok, err := c.SongByID(context.Background(), "de", "nope"); ok || err != nil {
		t.Errorf("missing: %v, %v", ok, err)
	}
}

func TestCatalog_refusedTokenIsRefetchedOnce(t *testing.T) {
	var calls atomic.Int32
	c, tok := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer tok1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"results":{}}`)
	}))
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); err != nil {
		t.Fatal(err)
	}
	if tok.invalidated.Load() != 1 || calls.Load() != 2 {
		t.Errorf("invalidated %d, calls %d", tok.invalidated.Load(), calls.Load())
	}

	c, _ = newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); !errors.Is(err, ErrDevTokenRefused) {
		t.Errorf("err = %v, want ErrDevTokenRefused", err)
	}
}

// Without RateLimitWait a 429 is never retried: retrying into Apple's window
// only prolongs it.
func TestCatalog_rateLimitStopsAtOnce(t *testing.T) {
	var calls atomic.Int32
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	_, err := c.SearchSongs(context.Background(), "ch", "secret", 5)
	var rl *RateLimitedError
	if !errors.Is(err, ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter != 2*time.Minute || calls.Load() != 1 ||
		strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v after %d calls, want ErrRateLimited after 1", err, calls.Load())
	}
}

func TestCatalog_retriesCutBodies(t *testing.T) {
	var calls atomic.Int32
	var waits []time.Duration
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			_, _ = fmt.Fprint(w, `{"results":{"songs":{"da`)
			return
		}
		_, _ = fmt.Fprint(w, `{"results":{}}`)
	}))
	c.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Errorf("calls %d, waits %v", calls.Load(), waits)
	}
	c, _ = newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `{`) }))
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("endless cut body: %v", err)
	}
	c.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled wait: %v", err)
	}
}

func TestCatalog_retriesABodyDroppedMidTransfer(t *testing.T) {
	var calls atomic.Int32
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "500")
			_, _ = fmt.Fprint(w, `{"results":{"songs":`)
			panic(http.ErrAbortHandler)
		}
		_, _ = fmt.Fprintf(w, `{"results":{"songs":{"data":[%s]}}}`, song("7", "Arrival", "1976", true))
	}))
	got, err := c.SearchSongs(context.Background(), "ch", "x", 5)
	if err != nil || len(got) != 1 || calls.Load() != 2 {
		t.Errorf("%v, %v after %d calls", got, err, calls.Load())
	}
}

// A 200 whose body does not decode is evicted before the retry, so a caching
// transport cannot replay it.
func TestCatalog_evictsAnUndecodableAnswer(t *testing.T) {
	var calls atomic.Int32
	c, _ := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"results":`) // complete on the wire, broken JSON
			return
		}
		_, _ = fmt.Fprint(w, `{"results":{}}`)
	}))
	var evicted []string
	c.Evict = func(method, url string) error { evicted = append(evicted, method+" "+url); return nil }
	if _, err := c.SearchSongs(context.Background(), "ch", "a b", 5); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || !strings.HasPrefix(evicted[0], "GET "+c.BaseURL+"/v1/catalog/ch/search?") {
		t.Errorf("evicted %v", evicted)
	}

	calls.Store(0)
	c.Evict = func(string, string) error { return errors.New("disk full") }
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); err == nil || !strings.Contains(err.Error(), "evict") {
		t.Errorf("evict failure: %v", err)
	}
}

// A network error Retryable rejects (reworm's offline cache miss) fails at
// once; the others are tried Tries times.
func TestCatalog_networkErrorsAndRetryable(t *testing.T) {
	var waits int
	down := &Client{BaseURL: "http://127.0.0.1:1", Dev: &fakeTokens{}, Tries: 3,
		Sleep: func(context.Context, time.Duration) error { waits++; return nil }}
	if _, err := down.SearchSongs(context.Background(), "ch", "x", 5); err == nil || !strings.Contains(err.Error(), "after 3 tries") || waits != 2 {
		t.Errorf("unreachable: %v after %d waits", err, waits)
	}
	waits = 0
	down.Retryable = func(error) bool { return false }
	if _, err := down.SearchSongs(context.Background(), "ch", "x", 5); err == nil || strings.Contains(err.Error(), "tries") || waits != 0 {
		t.Errorf("not retryable: %v after %d waits", err, waits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := down.SearchSongs(ctx, "ch", "x", 5); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestCatalog_errors(t *testing.T) {
	c, tok := newCatalog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"errors":[{"title":"Invalid Parameter","detail":"limit"}]}`)
	}))
	if _, err := c.SearchSongs(context.Background(), "ch", "secret words", 5); !errors.Is(err, ErrBadRequest) ||
		!strings.Contains(err.Error(), "Invalid Parameter: limit") || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v", err)
	}
	tok.err = errors.New("no token")
	if _, err := c.SearchSongs(context.Background(), "ch", "x", 5); err == nil || err.Error() != "no token" {
		t.Errorf("token err = %v", err)
	}
	if apiError([]byte(`{"errors":[{"title":"T"}]}`)) != "T" || apiError([]byte("x")) != "no error detail" {
		t.Error("apiError")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&Client{}).sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep: %v", err)
	}
	if err := (&Client{}).sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleep: %v", err)
	}
	if (&Client{}).base() != DefaultBaseURL {
		t.Error("default base")
	}
}
