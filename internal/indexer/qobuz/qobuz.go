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
		appID = "305874112" // Default fallback app_id
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
	ID                  string `json:"id"`
	Title               string `json:"title"`
	Version             string `json:"version"`
	ReleaseDateOriginal string `json:"release_date_original"`
	ReleaseDateDownload string `json:"release_date_download"`
	MaximumBitDepth     int    `json:"maximum_bit_depth"`
	MaximumSamplingRate float64 `json:"maximum_sampling_rate"`
	TracksCount         int    `json:"tracks_count"`
	UPC                 string `json:"upc"`
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
			ID          int64   `json:"id"`
			Title       string  `json:"title"`
			Version     string  `json:"version"`
			Duration    int     `json:"duration"`
			TrackNumber int     `json:"track_number"`
			MediaNumber int     `json:"media_number"`
			ISRC        string  `json:"isrc"`
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
