// Package applemusic reads the Apple Music API the way the music.apple.com web
// player does: the player's developer token (Source finds it in the player's
// JavaScript), the user's media-user-token for library calls, and the player's
// Origin. Access is unofficial and can break any time. It only reads: there is
// no method that sends anything but GET. loom's tunewire reads the user's
// library through it, reworm the catalog.
package applemusic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the API host the web player talks to.
const DefaultBaseURL = "https://amp-api.music.apple.com"

const (
	origin   = "https://music.apple.com"
	pageSize = 100
	// MaxPlaylists bounds one paged read of the library: the pages go out one
	// after another inside one tool call's deadline. Past it the read stops,
	// truncated.
	MaxPlaylists = 20 * pageSize
	// MaxTracks bounds one playlist's read the same way; the tool's text holds
	// about 250 track lines anyway.
	MaxTracks        = 3 * pageSize
	maxResponseBytes = 8 << 20
	// defaultRateLimitWait is a 429's wait when Apple sends no Retry-After and
	// Client.RateLimitWait allows one.
	defaultRateLimitWait = 2 * time.Second
	// probePath is a cheap catalog read that needs only the developer token: it
	// tells a refused developer token from a refused user token.
	probePath = "/v1/storefronts/us"
)

var (
	// ErrUserTokenExpired means Apple refused the media-user-token: expired,
	// revoked, or never valid. Only a new one from the user helps.
	ErrUserTokenExpired = errors.New("apple music refused the media-user-token")
	// ErrDevTokenRefused means Apple refused the web player's developer token
	// even freshly fetched: Apple changed something; the user's token is not at
	// fault.
	ErrDevTokenRefused = errors.New("apple music refused the web player's developer token")
	// ErrNotFound is a 404: no such playlist.
	ErrNotFound = errors.New("apple music: not found")
	// ErrBadRequest is a 400: Apple refused the request's shape (a limit or an
	// include it does not take).
	ErrBadRequest = errors.New("apple music: bad request")
	// ErrRateLimited is a 429 (a *RateLimitedError). Apple's window lasted
	// ~3.5 h on 2026-11-04: retrying into it only prolongs it, so a client
	// waits at most once (Client.RateLimitWait).
	ErrRateLimited = errors.New("apple music: rate limited (429)")
)

// RateLimitedError is a 429; RetryAfter is Apple's Retry-After, 0 when it
// sent none. errors.Is ErrRateLimited.
type RateLimitedError struct {
	Path       string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() + ": " + e.Path }

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitedError) Is(target error) bool { return target == ErrRateLimited }

// TokenSource hands out the developer token; *Source implements it.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(refused string)
}

// Client calls the API. BaseURL and HTTP default when empty; the zero policy
// is one try and no wait on a 429, for a caller someone waits on.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Dev     TokenSource
	// Tries bounds the attempts per request on a network error, a body cut
	// off mid-transfer (Apple does that) or a 200 that does not decode, with
	// 1 s, 2 s, 4 s … between them; 0 is 1. A batch job (reworm's import)
	// takes 4.
	Tries int
	// Retryable, when set, says whether a network error is worth another
	// try; nil retries all. reworm's offline cache answers a miss with an
	// error no retry fixes.
	Retryable func(error) bool
	// Evict, when set, drops a cached answer the client could not decode,
	// before the retry, so a caching transport cannot replay it.
	Evict func(method, url string) error
	// RateLimitWait is the most a 429 makes a call wait (Retry-After, 2 s
	// without one) before its one retry; 0 does not wait and returns a
	// *RateLimitedError at once.
	RateLimitWait time.Duration
	// Sleep waits between tries; tests replace it.
	Sleep func(context.Context, time.Duration) error
}

// Playlist is one library playlist.
type Playlist struct {
	ID           string
	Name         string
	Description  string
	DateAdded    string
	LastModified string
	Artwork      string
}

// URL opens the playlist in the web player.
func (p Playlist) URL() string { return origin + "/library/playlist/" + url.PathEscape(p.ID) }

// Track is one entry of a library playlist.
type Track struct {
	Name     string
	Artist   string
	Album    string
	Duration time.Duration
}

// Item is one catalog search hit, or one entry of the user's listening lists.
type Item struct {
	ID       string // listening lists only; search leaves it empty
	Kind     string // song, album, artist, playlist, station, music video
	Name     string
	Artist   string
	Album    string
	Released string
	Added    string // library entries: when the user added it
	Duration time.Duration
	URL      string
	Artwork  string
	// ArtistID is the catalog id of the item's (first) artist, when Apple
	// sent the artists relationship.
	ArtistID string
}

// Storefront returns the account's storefront ("ch", "us", …). It doubles as
// the check that a pasted media-user-token works.
func (c *Client) Storefront(ctx context.Context, userToken string) (string, error) {
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.get(ctx, userToken, "/v1/me/storefront", &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 || page.Data[0].ID == "" {
		return "", errors.New("apple music: storefront response without an id")
	}
	return page.Data[0].ID, nil
}

type playlistAttrs struct {
	Name        string `json:"name"`
	Description struct {
		Standard string `json:"standard"`
	} `json:"description"`
	DateAdded        string  `json:"dateAdded"`
	LastModifiedDate string  `json:"lastModifiedDate"`
	Artwork          artwork `json:"artwork"`
}

func (a playlistAttrs) playlist(id string) Playlist {
	return Playlist{ID: id, Name: a.Name, Description: a.Description.Standard, DateAdded: a.DateAdded,
		LastModified: a.LastModifiedDate, Artwork: a.Artwork.at(artworkSize)}
}

// Playlists lists the user's library playlists, at most MaxPlaylists;
// truncated says the library holds more.
func (c *Client) Playlists(ctx context.Context, userToken string) (list []Playlist, truncated bool, err error) {
	type entry struct {
		ID         string        `json:"id"`
		Attributes playlistAttrs `json:"attributes"`
	}
	truncated, err = pages(ctx, c, userToken, "/v1/me/library/playlists", pageSize, MaxPlaylists, func(e entry) {
		list = append(list, e.Attributes.playlist(e.ID))
	})
	return list, truncated, err
}

// LibrarySize counts the library playlists with one request: Apple's
// meta.total. exact is false when Apple sent no total (the count is then
// unknown); a 404 is an empty library. It doubles as a cheap check that the
// user token reads the library.
func (c *Client) LibrarySize(ctx context.Context, userToken string) (n int, exact bool, err error) {
	var page struct {
		Data []json.RawMessage `json:"data"`
		Next string            `json:"next"`
		Meta struct {
			Total *int `json:"total"`
		} `json:"meta"`
	}
	if err := c.get(ctx, userToken, "/v1/me/library/playlists?limit=1", &page); err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, true, nil
		}
		return 0, false, err
	}
	switch {
	case page.Meta.Total != nil:
		return *page.Meta.Total, true, nil
	case page.Next == "":
		return len(page.Data), true, nil
	default:
		return 0, false, nil
	}
}

// Playlist reads one library playlist by id; ErrNotFound when there is none.
func (c *Client) Playlist(ctx context.Context, userToken, playlistID string) (Playlist, error) {
	var page struct {
		Data []struct {
			ID         string        `json:"id"`
			Attributes playlistAttrs `json:"attributes"`
		} `json:"data"`
	}
	if err := c.get(ctx, userToken, "/v1/me/library/playlists/"+url.PathEscape(playlistID), &page); err != nil {
		return Playlist{}, err
	}
	if len(page.Data) == 0 {
		return Playlist{}, ErrNotFound
	}
	return page.Data[0].Attributes.playlist(page.Data[0].ID), nil
}

// PlaylistTracks lists one library playlist's tracks in order, at most
// MaxTracks; truncated says the playlist holds more.
func (c *Client) PlaylistTracks(ctx context.Context, userToken, playlistID string) (tracks []Track, truncated bool, err error) {
	type attrs struct {
		Name             string `json:"name"`
		ArtistName       string `json:"artistName"`
		AlbumName        string `json:"albumName"`
		DurationInMillis int64  `json:"durationInMillis"`
	}
	type entry struct {
		Attributes attrs `json:"attributes"`
	}
	path := "/v1/me/library/playlists/" + url.PathEscape(playlistID) + "/tracks"
	truncated, err = pages(ctx, c, userToken, path, pageSize, MaxTracks, func(e entry) {
		a := e.Attributes
		tracks = append(tracks, Track{Name: a.Name, Artist: a.ArtistName, Album: a.AlbumName, Duration: time.Duration(a.DurationInMillis) * time.Millisecond})
	})
	return tracks, truncated, err
}

// SearchKinds are the catalog types Search accepts.
var SearchKinds = []string{"songs", "albums", "artists"}

// Search looks term up in a storefront's catalog. It needs no user token.
func (c *Client) Search(ctx context.Context, storefront, term string, kinds []string, limit int) ([]Item, error) {
	q := url.Values{}
	q.Set("term", term)
	q.Set("types", strings.Join(kinds, ","))
	q.Set("limit", strconv.Itoa(limit))
	type hit struct {
		Attributes struct {
			Name             string  `json:"name"`
			ArtistName       string  `json:"artistName"`
			AlbumName        string  `json:"albumName"`
			ReleaseDate      string  `json:"releaseDate"`
			DurationInMillis int64   `json:"durationInMillis"`
			URL              string  `json:"url"`
			Artwork          artwork `json:"artwork"`
		} `json:"attributes"`
	}
	var body struct {
		Results map[string]struct {
			Data []hit `json:"data"`
		} `json:"results"`
	}
	path := "/v1/catalog/" + url.PathEscape(storefront) + "/search?" + q.Encode()
	if err := c.get(ctx, "", path, &body); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []Item
	for _, kind := range kinds {
		for _, h := range body.Results[kind].Data {
			a := h.Attributes
			out = append(out, Item{
				Kind: strings.TrimSuffix(kind, "s"), Name: a.Name, Artist: a.ArtistName, Album: a.AlbumName,
				Released: a.ReleaseDate, Duration: time.Duration(a.DurationInMillis) * time.Millisecond, URL: a.URL,
				Artwork: a.Artwork.at(artworkSize),
			})
		}
	}
	return out, nil
}

// artworkSize is the square edge, in pixels, of the cover a source shows.
const artworkSize = 300

// artwork is Apple's image template: a URL with {w} and {h} placeholders.
type artwork struct {
	URL string `json:"url"`
}

// at fills the template for a size×size image; only an https URL is kept.
func (a artwork) at(size int) string {
	if !strings.HasPrefix(a.URL, "https://") {
		return ""
	}
	n := strconv.Itoa(size)
	return strings.NewReplacer("{w}", n, "{h}", n, "{f}", "jpg").Replace(a.URL)
}

// pages follows a resource list's next links, handing each resource to add,
// until the list ends or max resources came in (truncated: there are more).
// size is the endpoint's page size: Apple caps it per endpoint, and a limit
// past the cap is a 400. A 404 on the first page is an empty list (Apple's
// answer for an empty library).
func pages[D any](ctx context.Context, c *Client, userToken, path string, size, max int, add func(D)) (truncated bool, err error) {
	return pagesOf(ctx, c, userToken, path, size, max, true, add)
}

// pagesOf is pages; emptyOn404 false makes a 404 on the first page
// ErrNotFound instead of an empty list (a catalog resource that is not there,
// not an empty library).
func pagesOf[D any](ctx context.Context, c *Client, userToken, path string, size, max int, emptyOn404 bool, add func(D)) (truncated bool, err error) {
	path += sep(path) + "limit=" + strconv.Itoa(size)
	_, query, _ := strings.Cut(path, "?")
	want, _ := url.ParseQuery(query)
	for n, first := 0, true; path != ""; first = false {
		var page struct {
			Data []D    `json:"data"`
			Next string `json:"next"`
		}
		if err := c.get(ctx, userToken, path, &page); err != nil {
			if first && emptyOn404 && errors.Is(err, ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		for _, d := range page.Data {
			if n == max {
				return true, nil
			}
			add(d)
			n++
		}
		path = page.Next
		if path != "" && n == max {
			return true, nil
		}
		if path != "" && !strings.HasPrefix(path, "/v1/") {
			return false, fmt.Errorf("apple music: unexpected next link %q", path)
		}
		if path != "" {
			path = carry(path, want)
		}
	}
	return false, nil
}

// carry adds the first request's query parameters (limit, include) that a
// next link left out: Apple's next links carry the offset, not always the
// rest.
func carry(next string, want url.Values) string {
	_, query, _ := strings.Cut(next, "?")
	have, _ := url.ParseQuery(query)
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !have.Has(k) {
			next += sep(next) + url.QueryEscape(k) + "=" + url.QueryEscape(want.Get(k))
		}
	}
	return next
}

func sep(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}

// get fetches path into out; a 404 is ErrNotFound. A 401 or 403 has two
// suspects. With a user token, a probe with the developer token alone
// decides: it passes, so the user token is refused (ErrUserTokenExpired) and
// the developer token stays; it fails, so the developer token is refetched
// once and the call retried. Refused again (or without a user token), it is
// ErrDevTokenRefused. A 429 waits once when RateLimitWait allows, else (or
// again) it is a *RateLimitedError. A network error, a cut-off body or a 200
// that does not decode is tried Tries times in all; the undecodable 200 is
// evicted first.
func (c *Client) get(ctx context.Context, userToken, path string, out any) error {
	refetched, waited := false, false
	tries := max(c.Tries, 1)
	for attempt := 1; ; attempt++ {
		dev, err := c.Dev.Token(ctx)
		if err != nil {
			return err
		}
		status, header, body, err := c.do(ctx, dev, userToken, path)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if attempt >= tries || (c.Retryable != nil && !c.Retryable(err)) {
				if attempt > 1 {
					return fmt.Errorf("%w after %d tries", err, attempt)
				}
				return err
			}
		case status == http.StatusOK:
			err := json.Unmarshal(body, out)
			if err == nil {
				return nil
			}
			if c.Evict != nil {
				if err := c.Evict(http.MethodGet, c.base()+path); err != nil {
					return fmt.Errorf("apple music: evict %s: %w", redact(path), err)
				}
			}
			if attempt >= tries {
				return fmt.Errorf("apple music: decode %s: %w", redact(path), err)
			}
		case status == http.StatusNotFound:
			return ErrNotFound
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			if userToken != "" {
				switch c.probeDevToken(ctx, dev) {
				case devTokenAccepted:
					return ErrUserTokenExpired
				case devTokenUnknown:
					// A 429, a 5xx or a network blip on the probe proves nothing:
					// keep the developer token, blame neither.
					return fmt.Errorf("apple music: %s returned %d and the check of the developer token failed", redact(path), status)
				}
			}
			if refetched {
				return ErrDevTokenRefused
			}
			c.Dev.Invalidate(dev)
			refetched = true
			continue
		case status == http.StatusTooManyRequests:
			ra := retryAfter(header.Get("Retry-After"))
			if waited || c.RateLimitWait <= 0 {
				return &RateLimitedError{Path: redact(path), RetryAfter: ra}
			}
			waited = true
			if ra == 0 {
				ra = defaultRateLimitWait
			}
			if err := c.sleep(ctx, min(ra, c.RateLimitWait)); err != nil {
				return err
			}
			continue
		case status == http.StatusBadRequest:
			return fmt.Errorf("%w: %s: %s", ErrBadRequest, redact(path), apiError(body))
		default:
			return fmt.Errorf("apple music: %s returned %d: %s", redact(path), status, apiError(body))
		}
		if err := c.sleep(ctx, time.Second<<(attempt-1)); err != nil {
			return err
		}
	}
}

func (c *Client) base() string {
	if base := strings.TrimRight(c.BaseURL, "/"); base != "" {
		return base
	}
	return DefaultBaseURL
}

type probeResult int

const (
	devTokenAccepted probeResult = iota
	devTokenRefused
	devTokenUnknown
)

// probeDevToken asks Apple whether it accepts dev on a read that needs no user
// token. Only a 401/403 counts as refused; anything else but a 200 is unknown.
func (c *Client) probeDevToken(ctx context.Context, dev string) probeResult {
	status, _, _, err := c.do(ctx, dev, "", probePath)
	switch {
	case err != nil:
		return devTokenUnknown
	case status == http.StatusOK:
		return devTokenAccepted
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return devTokenRefused
	default:
		return devTokenUnknown
	}
}

func (c *Client) do(ctx context.Context, dev, userToken, path string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+dev)
	if userToken != "" {
		req.Header.Set("Media-User-Token", userToken)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("apple music: %s: %w", redact(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("apple music: %s: read: %w", redact(path), err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfter reads a Retry-After of seconds; 0 when there is none.
func retryAfter(h string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// redact drops the query: a search term is the user's words and stays out of
// error text and logs.
func redact(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

// apiError pulls Apple's error title out of a response body.
func apiError(body []byte) string {
	var e struct {
		Errors []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Errors) > 0 {
		if e.Errors[0].Detail != "" {
			return e.Errors[0].Title + ": " + e.Errors[0].Detail
		}
		return e.Errors[0].Title
	}
	return "no error detail"
}
