// Package spotify reads public Spotify playlists so findr can import them.
//
// Two sources are used:
//   - The Web API (client-credentials flow) when a client ID and secret are
//     configured. Since Spotify's February 2026 Development Mode changes the
//     playlist entries live under /playlists/{id}/items and are only returned for
//     some playlists, so an empty result falls through to the embed page.
//   - The public embed page (open.spotify.com/embed/playlist/{id}), which needs
//     no credentials but lists at most 100 tracks and carries no ISRCs.
package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase   = "https://api.spotify.com/v1"
	defaultTokenURL  = "https://accounts.spotify.com/api/token"
	defaultEmbedBase = "https://open.spotify.com/embed/playlist/"
	embedTrackLimit  = 100
	browserUA        = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
)

// Track is one playlist entry.
type Track struct {
	URI         string   `json:"uri"`
	Title       string   `json:"title"`
	Artists     []string `json:"artists"`
	Album       string   `json:"album,omitempty"`
	DurationSec int      `json:"duration_sec"`
	ISRC        string   `json:"isrc,omitempty"`
}

// Playlist is a fetched Spotify playlist.
type Playlist struct {
	ID       string  `json:"id"`
	URL      string  `json:"url"`
	Name     string  `json:"name"`
	Owner    string  `json:"owner,omitempty"`
	ImageURL string  `json:"image_url,omitempty"`
	Tracks   []Track `json:"tracks"`
	// Via is "api" or "embed".
	Via string `json:"via"`
	// Partial is true when the embed page hit its 100-track cap, so the
	// playlist may hold more tracks than were read.
	Partial bool `json:"partial"`
}

// Client fetches playlists. The zero value is not usable; call NewClient.
type Client struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	apiBase   string
	tokenURL  string
	embedBase string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewClient creates a client. Empty credentials disable the Web API path.
func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		httpClient:   &http.Client{Timeout: 20 * time.Second},
		apiBase:      defaultAPIBase,
		tokenURL:     defaultTokenURL,
		embedBase:    defaultEmbedBase,
	}
}

var (
	idRe       = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
	urlPathRe  = regexp.MustCompile(`/playlist/([A-Za-z0-9]{22})`)
	uriRe      = regexp.MustCompile(`^spotify:(?:user:[^:]+:)?playlist:([A-Za-z0-9]{22})$`)
	nextDataRe = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
)

// ParsePlaylistID accepts a playlist URL (any locale prefix, with or without
// query string), a spotify:playlist: URI, or a bare 22-character ID.
func ParsePlaylistID(input string) (string, error) {
	s := strings.TrimSpace(input)
	if idRe.MatchString(s) {
		return s, nil
	}
	if m := uriRe.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		host := strings.ToLower(u.Host)
		if host == "open.spotify.com" || host == "play.spotify.com" {
			if m := urlPathRe.FindStringSubmatch(u.Path); m != nil {
				return m[1], nil
			}
		}
	}
	return "", fmt.Errorf("not a Spotify playlist link: %q", input)
}

// GetPlaylist fetches a playlist by ID, preferring the Web API when configured.
func (c *Client) GetPlaylist(ctx context.Context, id string) (*Playlist, error) {
	var apiErr error
	if c.clientID != "" && c.clientSecret != "" {
		pl, err := c.getPlaylistAPI(ctx, id)
		if err == nil && len(pl.Tracks) > 0 {
			return pl, nil
		}
		apiErr = err
		if apiErr == nil {
			apiErr = fmt.Errorf("web API returned no playlist items")
		}
	}

	pl, err := c.getPlaylistEmbed(ctx, id)
	if err != nil {
		if apiErr != nil {
			return nil, fmt.Errorf("spotify web API: %v; embed page: %w", apiErr, err)
		}
		return nil, err
	}
	return pl, nil
}

// ---- Web API ----

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("spotify token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spotify token request returned status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode spotify token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("spotify token response had no access_token")
	}
	c.token = tok.AccessToken
	// Refresh a minute early.
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	return c.token, nil
}

func (c *Client) apiGet(ctx context.Context, path string, out interface{}) (int, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("spotify GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, fmt.Errorf("spotify GET %s returned status %d", path, resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}

type apiTrack struct {
	Type       string `json:"type"`
	URI        string `json:"uri"`
	Name       string `json:"name"`
	DurationMs int    `json:"duration_ms"`
	ExternalID struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		Name string `json:"name"`
	} `json:"album"`
}

// apiEntry covers both the pre-2026 ("track") and current ("item") entry shapes.
type apiEntry struct {
	Track *apiTrack `json:"track"`
	Item  *apiTrack `json:"item"`
}

type apiPage struct {
	Items []apiEntry `json:"items"`
	Next  string     `json:"next"`
	Total int        `json:"total"`
}

func (c *Client) getPlaylistAPI(ctx context.Context, id string) (*Playlist, error) {
	var meta struct {
		Name  string `json:"name"`
		Owner struct {
			DisplayName string `json:"display_name"`
		} `json:"owner"`
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
		ExternalURLs struct {
			Spotify string `json:"spotify"`
		} `json:"external_urls"`
	}
	if _, err := c.apiGet(ctx, "/playlists/"+id+"?fields=name,owner(display_name),images,external_urls", &meta); err != nil {
		return nil, err
	}

	pl := &Playlist{
		ID:    id,
		URL:   meta.ExternalURLs.Spotify,
		Name:  meta.Name,
		Owner: meta.Owner.DisplayName,
		Via:   "api",
	}
	if pl.URL == "" {
		pl.URL = "https://open.spotify.com/playlist/" + id
	}
	if len(meta.Images) > 0 {
		pl.ImageURL = meta.Images[0].URL
	}

	// Current endpoint is /items; older apps may still be served /tracks.
	endpoints := []string{"items", "tracks"}
	var lastErr error
	for _, ep := range endpoints {
		tracks, status, err := c.pageAPITracks(ctx, id, ep)
		if err == nil {
			pl.Tracks = tracks
			return pl, nil
		}
		lastErr = err
		if status != http.StatusNotFound && status != http.StatusForbidden {
			break
		}
	}
	return pl, lastErr
}

func (c *Client) pageAPITracks(ctx context.Context, id, endpoint string) ([]Track, int, error) {
	var tracks []Track
	for offset := 0; offset < 10000; offset += 100 {
		var page apiPage
		path := fmt.Sprintf("/playlists/%s/%s?limit=100&offset=%d", id, endpoint, offset)
		status, err := c.apiGet(ctx, path, &page)
		if err != nil {
			return nil, status, err
		}
		for _, e := range page.Items {
			t := e.Item
			if t == nil {
				t = e.Track
			}
			if t == nil || t.Name == "" || (t.Type != "" && t.Type != "track") {
				continue // removed tracks, podcast episodes
			}
			tr := Track{
				URI:         t.URI,
				Title:       t.Name,
				Album:       t.Album.Name,
				DurationSec: t.DurationMs / 1000,
				ISRC:        strings.ToUpper(strings.TrimSpace(t.ExternalID.ISRC)),
			}
			for _, a := range t.Artists {
				if a.Name != "" {
					tr.Artists = append(tr.Artists, a.Name)
				}
			}
			tracks = append(tracks, tr)
		}
		if page.Next == "" || len(page.Items) == 0 {
			break
		}
	}
	return tracks, http.StatusOK, nil
}

// ---- Embed page ----

type embedData struct {
	Props struct {
		PageProps struct {
			State struct {
				Data struct {
					Entity struct {
						Name     string `json:"name"`
						Subtitle string `json:"subtitle"`
						CoverArt struct {
							Sources []struct {
								URL string `json:"url"`
							} `json:"sources"`
						} `json:"coverArt"`
						TrackList []struct {
							URI        string `json:"uri"`
							Title      string `json:"title"`
							Subtitle   string `json:"subtitle"`
							Duration   int    `json:"duration"`
							EntityType string `json:"entityType"`
						} `json:"trackList"`
					} `json:"entity"`
				} `json:"data"`
			} `json:"state"`
		} `json:"pageProps"`
	} `json:"props"`
}

func (c *Client) getPlaylistEmbed(ctx context.Context, id string) (*Playlist, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.embedBase+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "en")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch spotify embed page: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("playlist %s not found or not public", id)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify embed page returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read spotify embed page: %w", err)
	}
	return parseEmbed(id, body)
}

func parseEmbed(id string, body []byte) (*Playlist, error) {
	m := nextDataRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("spotify embed page had no playlist data (layout may have changed)")
	}
	var d embedData
	if err := json.Unmarshal(m[1], &d); err != nil {
		return nil, fmt.Errorf("decode spotify embed data: %w", err)
	}
	e := d.Props.PageProps.State.Data.Entity
	if e.Name == "" && len(e.TrackList) == 0 {
		return nil, fmt.Errorf("playlist %s not found or not public", id)
	}

	pl := &Playlist{
		ID:      id,
		URL:     "https://open.spotify.com/playlist/" + id,
		Name:    e.Name,
		Owner:   e.Subtitle,
		Via:     "embed",
		Partial: len(e.TrackList) >= embedTrackLimit,
	}
	if len(e.CoverArt.Sources) > 0 {
		pl.ImageURL = e.CoverArt.Sources[0].URL
	}
	for _, t := range e.TrackList {
		if t.Title == "" || (t.EntityType != "" && t.EntityType != "track") {
			continue
		}
		pl.Tracks = append(pl.Tracks, Track{
			URI:         t.URI,
			Title:       t.Title,
			Artists:     splitArtists(t.Subtitle),
			DurationSec: t.Duration / 1000,
		})
	}
	return pl, nil
}

func splitArtists(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.ReplaceAll(p, " ", " "))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
