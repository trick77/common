package applemusic

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// maxSongPages bounds one song list: Apple pages filter[isrc] at 25, a hit
// song has a few pages of compilations.
const maxSongPages = 8

// Song is one catalog song with what matching it needs (reworm's import).
type Song struct {
	ID     string
	Name   string
	Artist string
	Album  string
	// AlbumArtist is the album's artist: the song's own for its albums,
	// "Various Artists" (localized) for the playlist-like compilations that
	// crowd an ISRC lookup.
	AlbumArtist string
	ReleaseDate string // YYYY-MM-DD or YYYY
	ISRC        string
	// Artwork is Apple's image template with its {w}x{h} placeholders,
	// https only.
	Artwork string
	// Preview says Apple ships a preview clip; PreviewURL is it. It changes:
	// read it when it is played, never keep it.
	Preview    bool
	PreviewURL string
}

// Year is the release year, 0 when Apple sent none.
func (s Song) Year() int {
	if len(s.ReleaseDate) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(s.ReleaseDate[:4])
	return y
}

type songResource struct {
	ID         string `json:"id"`
	Attributes struct {
		Name            string `json:"name"`
		ArtistName      string `json:"artistName"`
		AlbumName       string `json:"albumName"`
		AlbumArtistName string `json:"albumArtistName"`
		ReleaseDate     string `json:"releaseDate"`
		ISRC            string `json:"isrc"`
		Artwork         struct {
			URL string `json:"url"`
		} `json:"artwork"`
		Previews []struct {
			URL string `json:"url"`
		} `json:"previews"`
	} `json:"attributes"`
}

func (r songResource) song() Song {
	a := r.Attributes
	art := a.Artwork.URL
	if !strings.HasPrefix(art, "https://") {
		art = ""
	}
	s := Song{ID: r.ID, Name: a.Name, Artist: a.ArtistName, Album: a.AlbumName, AlbumArtist: a.AlbumArtistName,
		ReleaseDate: a.ReleaseDate, ISRC: a.ISRC, Artwork: art}
	if len(a.Previews) > 0 && strings.HasPrefix(a.Previews[0].URL, "https://") {
		s.Preview, s.PreviewURL = true, a.Previews[0].URL
	}
	return s
}

// SongsByISRC lists a storefront's songs with any of the ISRCs. Apple answers
// at most 25 per ISRC, without a next link or offset (checked 2026-11-03): a
// hit's original album can be missing, AlbumTracks finds it. Next links are
// followed should Apple ever send them.
func (c *Client) SongsByISRC(ctx context.Context, storefront string, isrcs []string) ([]Song, error) {
	if len(isrcs) == 0 {
		return nil, nil
	}
	q := url.Values{}
	q.Set("filter[isrc]", strings.Join(isrcs, ","))
	return c.songPages(ctx, "/v1/catalog/"+url.PathEscape(storefront)+"/songs?"+q.Encode())
}

// SongByID reads one catalog song; found is false when the storefront has no
// such song.
func (c *Client) SongByID(ctx context.Context, storefront, id string) (s Song, found bool, err error) {
	songs, err := c.songPages(ctx, "/v1/catalog/"+url.PathEscape(storefront)+"/songs/"+url.PathEscape(id))
	if err != nil || len(songs) == 0 {
		return Song{}, false, err
	}
	return songs[0], true, nil
}

// AlbumTracks lists an album's songs (music videos left out).
func (c *Client) AlbumTracks(ctx context.Context, storefront, albumID string) ([]Song, error) {
	return c.songPages(ctx, "/v1/catalog/"+url.PathEscape(storefront)+"/albums/"+url.PathEscape(albumID)+"/tracks?limit=100")
}

// songPages reads a song list; a 404 is an empty one.
func (c *Client) songPages(ctx context.Context, path string) ([]Song, error) {
	var out []Song
	for n := 0; path != "" && n < maxSongPages; n++ {
		var page struct {
			Data []struct {
				Type string `json:"type"`
				songResource
			} `json:"data"`
			Next string `json:"next"`
		}
		if err := c.get(ctx, "", path, &page); err != nil {
			if errors.Is(err, ErrNotFound) {
				break
			}
			return nil, err
		}
		for _, d := range page.Data {
			if d.Type == "" || d.Type == "songs" {
				out = append(out, d.song())
			}
		}
		path = page.Next
		if path != "" && !strings.HasPrefix(path, "/v1/") {
			return nil, fmt.Errorf("apple music: unexpected next link %q", path)
		}
	}
	return out, nil
}

// Album is one catalog album.
type Album struct {
	ID          string
	Name        string
	Artist      string
	ReleaseDate string
}

// Year is the release year, 0 when Apple sent none.
func (a Album) Year() int { return Song{ReleaseDate: a.ReleaseDate}.Year() }

// SearchAlbums looks term up among a storefront's albums.
func (c *Client) SearchAlbums(ctx context.Context, storefront, term string, limit int) ([]Album, error) {
	var body struct {
		Results struct {
			Albums struct {
				Data []struct {
					ID         string `json:"id"`
					Attributes struct {
						Name        string `json:"name"`
						ArtistName  string `json:"artistName"`
						ReleaseDate string `json:"releaseDate"`
					} `json:"attributes"`
				} `json:"data"`
			} `json:"albums"`
		} `json:"results"`
	}
	if err := c.search(ctx, storefront, term, "albums", limit, &body); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(body.Results.Albums.Data))
	for _, d := range body.Results.Albums.Data {
		a := d.Attributes
		out = append(out, Album{ID: d.ID, Name: a.Name, Artist: a.ArtistName, ReleaseDate: a.ReleaseDate})
	}
	return out, nil
}

// SearchSongs looks term up among a storefront's songs.
func (c *Client) SearchSongs(ctx context.Context, storefront, term string, limit int) ([]Song, error) {
	var body struct {
		Results struct {
			Songs struct {
				Data []songResource `json:"data"`
			} `json:"songs"`
		} `json:"results"`
	}
	if err := c.search(ctx, storefront, term, "songs", limit, &body); err != nil {
		return nil, err
	}
	out := make([]Song, 0, len(body.Results.Songs.Data))
	for _, d := range body.Results.Songs.Data {
		out = append(out, d.song())
	}
	return out, nil
}

// search runs one catalog search into out; a 404 leaves out empty.
func (c *Client) search(ctx context.Context, storefront, term, types string, limit int, out any) error {
	q := url.Values{}
	q.Set("term", term)
	q.Set("types", types)
	q.Set("limit", strconv.Itoa(limit))
	err := c.get(ctx, "", "/v1/catalog/"+url.PathEscape(storefront)+"/search?"+q.Encode(), out)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
