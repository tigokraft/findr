package qobuz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/indexer"
)

const (
	defaultBaseURL = "https://www.qobuz.com/api.json/0.2"
	clientTimeout  = 10 * time.Second
)

// Client implements indexer.Indexer using Qobuz's catalog API.
type Client struct {
	baseURL    string
	appID      string
	httpClient *http.Client
}

// NewClient initializes a Qobuz catalog indexer.
func NewClient(appID string) *Client {
	if appID == "" {
		appID = "798273057" // Standard Qobuz client ID
	}
	return &Client{
		baseURL: defaultBaseURL,
		appID:   appID,
		httpClient: &http.Client{
			Timeout: clientTimeout,
		},
	}
}

func (c *Client) Name() string {
	return "qobuz"
}

type qobuzArtistSearchResponse struct {
	Artists struct {
		Items []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Picture string `json:"picture"`
		} `json:"items"`
		Total int `json:"total"`
	} `json:"artists"`
}

type qobuzArtistGetResponse struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
	Albums  struct {
		Items []qobuzAlbumItem `json:"items"`
		Total int              `json:"total"`
	} `json:"albums"`
}

type qobuzAlbumItem struct {
	ID                  string  `json:"id"`
	Title               string  `json:"title"`
	Version             string  `json:"version"`
	ReleaseDateOriginal string  `json:"release_date_original"`
	ReleaseDateDownload string  `json:"release_date_download"`
	MaximumBitDepth     int     `json:"maximum_bit_depth"`
	MaximumSamplingRate float64 `json:"maximum_sampling_rate"`
	TracksCount         int     `json:"tracks_count"`
	UPC                 string  `json:"upc"`
	Image               struct {
		Small     string `json:"small"`
		Thumbnail string `json:"thumbnail"`
		Large     string `json:"large"`
	} `json:"image"`
	Artist struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
}

type qobuzAlbumGetResponse struct {
	qobuzAlbumItem
	Tracks struct {
		Items []struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			Version     string `json:"version"`
			Duration    int    `json:"duration"`
			TrackNumber int    `json:"track_number"`
			MediaNumber int    `json:"media_number"`
			ISRC        string `json:"isrc"`
			Performer   struct {
				Name string `json:"name"`
			} `json:"performer"`
		} `json:"items"`
	} `json:"tracks"`
}

func (c *Client) SearchArtist(ctx context.Context, query string) ([]domain.Artist, error) {
	reqURL := fmt.Sprintf("%s/artist/search?query=%s&app_id=%s&limit=20", c.baseURL, url.QueryEscape(query), c.appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz search artist request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qobuz search returned status %d", resp.StatusCode)
	}

	var searchResp qobuzArtistSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("decode qobuz search response: %w", err)
	}

	var artists []domain.Artist
	for _, item := range searchResp.Artists.Items {
		artists = append(artists, domain.Artist{
			ID:        strconv.FormatInt(item.ID, 10),
			Name:      item.Name,
			ImageURL:  item.Picture,
			Source:    "qobuz",
			SourceID:  strconv.FormatInt(item.ID, 10),
			Monitored: false,
		})
	}

	return artists, nil
}

func (c *Client) GetArtist(ctx context.Context, id string) (*domain.Artist, error) {
	reqURL := fmt.Sprintf("%s/artist/get?artist_id=%s&app_id=%s", c.baseURL, id, c.appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz get artist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qobuz get artist returned status %d", resp.StatusCode)
	}

	var artistResp qobuzArtistGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&artistResp); err != nil {
		return nil, fmt.Errorf("decode qobuz artist response: %w", err)
	}

	return &domain.Artist{
		ID:        strconv.FormatInt(artistResp.ID, 10),
		Name:      artistResp.Name,
		ImageURL:  artistResp.Picture,
		Source:    "qobuz",
		SourceID:  strconv.FormatInt(artistResp.ID, 10),
		Monitored: false,
	}, nil
}

func (c *Client) GetArtistReleases(ctx context.Context, artistID string, filter indexer.ReleaseFilter) ([]domain.Album, error) {
	limit := 100
	if filter.Limit > 0 {
		limit = filter.Limit
	}

	reqURL := fmt.Sprintf("%s/artist/get?artist_id=%s&extra=albums&limit=%d&app_id=%s", c.baseURL, artistID, limit, c.appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz get releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qobuz get albums returned status %d", resp.StatusCode)
	}

	var artistResp qobuzArtistGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&artistResp); err != nil {
		return nil, fmt.Errorf("decode qobuz albums response: %w", err)
	}

	var albums []domain.Album
	for _, item := range artistResp.Albums.Items {
		relType := inferReleaseType(item.TracksCount)

		relDate := item.ReleaseDateOriginal
		if relDate == "" {
			relDate = item.ReleaseDateDownload
		}

		year := 0
		if len(relDate) >= 4 {
			year, _ = strconv.Atoi(relDate[:4])
		}

		title := item.Title
		if item.Version != "" && !strings.Contains(title, item.Version) {
			title = fmt.Sprintf("%s (%s)", title, item.Version)
		}

		albums = append(albums, domain.Album{
			ID:          item.ID,
			ArtistID:    artistID,
			ArtistName:  artistResp.Name,
			Title:       title,
			ReleaseType: relType,
			ReleaseDate: relDate,
			Year:        year,
			CoverURL:    item.Image.Large,
			TotalTracks: item.TracksCount,
			UPC:         item.UPC,
			Source:      "qobuz",
			SourceID:    item.ID,
			Status:      domain.StatusPending,
		})
	}

	return albums, nil
}

func (c *Client) GetAlbumDetails(ctx context.Context, albumID string) (*domain.Album, error) {
	reqURL := fmt.Sprintf("%s/album/get?album_id=%s&app_id=%s", c.baseURL, albumID, c.appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz get album: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qobuz get album returned status %d", resp.StatusCode)
	}

	var albumResp qobuzAlbumGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&albumResp); err != nil {
		return nil, fmt.Errorf("decode qobuz album response: %w", err)
	}

	relDate := albumResp.ReleaseDateOriginal
	if relDate == "" {
		relDate = albumResp.ReleaseDateDownload
	}

	year := 0
	if len(relDate) >= 4 {
		year, _ = strconv.Atoi(relDate[:4])
	}

	title := albumResp.Title
	if albumResp.Version != "" && !strings.Contains(title, albumResp.Version) {
		title = fmt.Sprintf("%s (%s)", title, albumResp.Version)
	}

	album := &domain.Album{
		ID:          albumResp.ID,
		ArtistID:    strconv.FormatInt(albumResp.Artist.ID, 10),
		ArtistName:  albumResp.Artist.Name,
		Title:       title,
		ReleaseType: inferReleaseType(albumResp.TracksCount),
		ReleaseDate: relDate,
		Year:        year,
		CoverURL:    albumResp.Image.Large,
		TotalTracks: albumResp.TracksCount,
		UPC:         albumResp.UPC,
		Source:      "qobuz",
		SourceID:    albumResp.ID,
		Status:      domain.StatusPending,
	}

	for _, t := range albumResp.Tracks.Items {
		trackTitle := t.Title
		if t.Version != "" && !strings.Contains(trackTitle, t.Version) {
			trackTitle = fmt.Sprintf("%s (%s)", trackTitle, t.Version)
		}

		perf := t.Performer.Name
		if perf == "" {
			perf = albumResp.Artist.Name
		}

		album.Tracks = append(album.Tracks, domain.Track{
			ID:          strconv.FormatInt(t.ID, 10),
			AlbumID:     album.ID,
			ArtistName:  perf,
			Title:       trackTitle,
			DiscNumber:  t.MediaNumber,
			TrackNumber: t.TrackNumber,
			DurationSec: t.Duration,
			ISRC:        t.ISRC,
			SourceID:    strconv.FormatInt(t.ID, 10),
		})
	}

	return album, nil
}

func inferReleaseType(trackCount int) domain.ReleaseType {
	switch {
	case trackCount <= 2:
		return domain.ReleaseTypeSingle
	case trackCount <= 6:
		return domain.ReleaseTypeEP
	default:
		return domain.ReleaseTypeAlbum
	}
}

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out interface{}) error {
	params.Set("app_id", c.appID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("qobuz request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qobuz %s returned status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func albumFromItem(item qobuzAlbumItem) domain.Album {
	relDate := item.ReleaseDateOriginal
	if relDate == "" {
		relDate = item.ReleaseDateDownload
	}
	year := 0
	if len(relDate) >= 4 {
		year, _ = strconv.Atoi(relDate[:4])
	}
	title := item.Title
	if item.Version != "" && !strings.Contains(title, item.Version) {
		title = fmt.Sprintf("%s (%s)", title, item.Version)
	}
	return domain.Album{
		ID:          item.ID,
		ArtistID:    strconv.FormatInt(item.Artist.ID, 10),
		ArtistName:  item.Artist.Name,
		Title:       title,
		ReleaseType: inferReleaseType(item.TracksCount),
		ReleaseDate: relDate,
		Year:        year,
		CoverURL:    item.Image.Large,
		TotalTracks: item.TracksCount,
		UPC:         item.UPC,
		Source:      "qobuz",
		SourceID:    item.ID,
		Status:      domain.StatusPending,
	}
}

// SearchAlbum searches the Qobuz catalog for releases.
func (c *Client) SearchAlbum(ctx context.Context, query string) ([]domain.Album, error) {
	var res struct {
		Albums struct {
			Items []qobuzAlbumItem `json:"items"`
		} `json:"albums"`
	}
	if err := c.getJSON(ctx, "/album/search", url.Values{"query": {query}, "limit": {"40"}}, &res); err != nil {
		return nil, err
	}
	albums := make([]domain.Album, 0, len(res.Albums.Items))
	for _, item := range res.Albums.Items {
		albums = append(albums, albumFromItem(item))
	}
	return albums, nil
}

// SearchTrack searches Qobuz tracks, returning each hit with its parent release.
func (c *Client) SearchTrack(ctx context.Context, artist, title string) ([]indexer.TrackHit, error) {
	var res struct {
		Tracks struct {
			Items []struct {
				ID        int64  `json:"id"`
				Title     string `json:"title"`
				Version   string `json:"version"`
				Duration  int    `json:"duration"`
				ISRC      string `json:"isrc"`
				Performer struct {
					Name string `json:"name"`
				} `json:"performer"`
				Album qobuzAlbumItem `json:"album"`
			} `json:"items"`
		} `json:"tracks"`
	}
	q := strings.TrimSpace(artist + " " + title)
	if err := c.getJSON(ctx, "/track/search", url.Values{"query": {q}, "limit": {"15"}}, &res); err != nil {
		return nil, err
	}
	hits := make([]indexer.TrackHit, 0, len(res.Tracks.Items))
	for _, t := range res.Tracks.Items {
		name := t.Title
		if t.Version != "" && !strings.Contains(name, t.Version) {
			name = fmt.Sprintf("%s (%s)", name, t.Version)
		}
		perf := t.Performer.Name
		if perf == "" {
			perf = t.Album.Artist.Name
		}
		hits = append(hits, indexer.TrackHit{
			TrackID:     strconv.FormatInt(t.ID, 10),
			Title:       name,
			ArtistName:  perf,
			DurationSec: t.Duration,
			ISRC:        t.ISRC,
			AlbumID:     t.Album.ID,
			AlbumTitle:  t.Album.Title,
			CoverURL:    t.Album.Image.Large,
		})
	}
	return hits, nil
}

// GetTrack fetches one track by its Qobuz ID.
func (c *Client) GetTrack(ctx context.Context, id string) (*indexer.TrackHit, error) {
	var t struct {
		ID        int64  `json:"id"`
		Title     string `json:"title"`
		Version   string `json:"version"`
		Duration  int    `json:"duration"`
		ISRC      string `json:"isrc"`
		Performer struct {
			Name string `json:"name"`
		} `json:"performer"`
		Album qobuzAlbumItem `json:"album"`
	}
	if err := c.getJSON(ctx, "/track/get", url.Values{"track_id": {id}}, &t); err != nil {
		return nil, err
	}
	if t.ID == 0 || t.Album.ID == "" {
		return nil, nil
	}
	name := t.Title
	if t.Version != "" && !strings.Contains(name, t.Version) {
		name = fmt.Sprintf("%s (%s)", name, t.Version)
	}
	perf := t.Performer.Name
	if perf == "" {
		perf = t.Album.Artist.Name
	}
	return &indexer.TrackHit{
		TrackID:     strconv.FormatInt(t.ID, 10),
		Title:       name,
		ArtistName:  perf,
		DurationSec: t.Duration,
		ISRC:        t.ISRC,
		AlbumID:     t.Album.ID,
		AlbumTitle:  t.Album.Title,
		CoverURL:    t.Album.Image.Large,
	}, nil
}
