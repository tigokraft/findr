package deezer

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
	defaultBaseURL = "https://api.deezer.com"
	clientTimeout  = 10 * time.Second
)

// Client implements the indexer.Indexer interface against Deezer's public API.
// Requires NO API credentials.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient initializes a new Deezer indexer client.
func NewClient() *Client {
	return &Client{
		baseURL: defaultBaseURL,
		httpClient: &http.Client{
			Timeout: clientTimeout,
		},
	}
}

func (c *Client) Name() string {
	return "deezer"
}

// --- Deezer JSON Models ---

type artistSearchResponse struct {
	Data []struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		PictureBig    string `json:"picture_big"`
		PictureMedium string `json:"picture_medium"`
		Link          string `json:"link"`
	} `json:"data"`
	Total int `json:"total"`
}

type artistDetailResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	PictureBig string `json:"picture_big"`
	Link       string `json:"link"`
	NbAlbum    int    `json:"nb_album"`
	NbFan      int    `json:"nb_fan"`
}

type albumListResponse struct {
	Data []struct {
		ID          int64  `json:"id"`
		Title       string `json:"title"`
		CoverXL     string `json:"cover_xl"`
		CoverBig    string `json:"cover_big"`
		ReleaseDate string `json:"release_date"`
		RecordType  string `json:"record_type"` // "album", "single", "ep", "compile"
		GenreID     int    `json:"genre_id"`
		Explicit    bool   `json:"explicit_lyrics"`
	} `json:"data"`
	Total int    `json:"total"`
	Next  string `json:"next"`
}

type albumDetailResponse struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	UPC         string `json:"upc"`
	CoverXL     string `json:"cover_xl"`
	CoverBig    string `json:"cover_big"`
	ReleaseDate string `json:"release_date"`
	RecordType  string `json:"record_type"`
	NbTracks    int    `json:"nb_tracks"`
	Artist      struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
	Tracks struct {
		Data []trackItemResponse `json:"data"`
	} `json:"tracks"`
}

type trackItemResponse struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	TitleVersion  string `json:"title_version"`
	ISRC          string `json:"isrc"`
	Duration      int    `json:"duration"`
	TrackPosition int    `json:"track_position"`
	DiskNumber    int    `json:"disk_number"`
	Explicit      bool   `json:"explicit_lyrics"`
	Artist        struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
}

// SearchArtist searches for artists by name.
func (c *Client) SearchArtist(ctx context.Context, query string) ([]domain.Artist, error) {
	reqURL := fmt.Sprintf("%s/search/artist?q=%s", c.baseURL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deezer search artist request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer search returned status %d", resp.StatusCode)
	}

	var searchResp artistSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("failed to decode deezer response: %w", err)
	}

	var artists []domain.Artist
	for _, item := range searchResp.Data {
		img := item.PictureBig
		if img == "" {
			img = item.PictureMedium
		}
		artists = append(artists, domain.Artist{
			ID:        strconv.FormatInt(item.ID, 10),
			Name:      item.Name,
			ImageURL:  img,
			Source:    "deezer",
			SourceID:  strconv.FormatInt(item.ID, 10),
			Monitored: false,
		})
	}

	return artists, nil
}

// GetArtist fetches an artist profile by ID.
func (c *Client) GetArtist(ctx context.Context, id string) (*domain.Artist, error) {
	reqURL := fmt.Sprintf("%s/artist/%s", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deezer get artist failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer get artist returned status %d", resp.StatusCode)
	}

	var artistResp artistDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&artistResp); err != nil {
		return nil, fmt.Errorf("decode artist response: %w", err)
	}

	return &domain.Artist{
		ID:        strconv.FormatInt(artistResp.ID, 10),
		Name:      artistResp.Name,
		ImageURL:  artistResp.PictureBig,
		Source:    "deezer",
		SourceID:  strconv.FormatInt(artistResp.ID, 10),
		Monitored: false,
	}, nil
}

// GetArtistReleases returns albums, EPs, and singles for an artist.
func (c *Client) GetArtistReleases(ctx context.Context, artistID string, filter indexer.ReleaseFilter) ([]domain.Album, error) {
	limit := 100
	if filter.Limit > 0 && filter.Limit < 100 {
		limit = filter.Limit
	}

	reqURL := fmt.Sprintf("%s/artist/%s/albums?limit=%d", c.baseURL, artistID, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deezer get artist releases failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer get albums returned status %d", resp.StatusCode)
	}

	var albumsResp albumListResponse
	if err := json.NewDecoder(resp.Body).Decode(&albumsResp); err != nil {
		return nil, fmt.Errorf("decode albums response: %w", err)
	}

	typeFilterMap := make(map[domain.ReleaseType]bool)
	for _, t := range filter.Types {
		typeFilterMap[t] = true
	}

	var albums []domain.Album
	for _, item := range albumsResp.Data {
		relType := mapRecordType(item.RecordType)
		if len(typeFilterMap) > 0 && !typeFilterMap[relType] {
			continue
		}

		cover := item.CoverXL
		if cover == "" {
			cover = item.CoverBig
		}

		year := 0
		if len(item.ReleaseDate) >= 4 {
			year, _ = strconv.Atoi(item.ReleaseDate[:4])
		}

		albums = append(albums, domain.Album{
			ID:          strconv.FormatInt(item.ID, 10),
			ArtistID:    artistID,
			Title:       item.Title,
			ReleaseType: relType,
			ReleaseDate: item.ReleaseDate,
			Year:        year,
			CoverURL:    cover,
			Source:      "deezer",
			SourceID:    strconv.FormatInt(item.ID, 10),
			Status:      domain.StatusPending,
		})
	}

	return albums, nil
}

// GetAlbumDetails fetches full track metadata for an album.
func (c *Client) GetAlbumDetails(ctx context.Context, albumID string) (*domain.Album, error) {
	reqURL := fmt.Sprintf("%s/album/%s", c.baseURL, albumID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deezer get album details failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer get album returned status %d", resp.StatusCode)
	}

	var albumResp albumDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&albumResp); err != nil {
		return nil, fmt.Errorf("decode album detail response: %w", err)
	}

	cover := albumResp.CoverXL
	if cover == "" {
		cover = albumResp.CoverBig
	}

	year := 0
	if len(albumResp.ReleaseDate) >= 4 {
		year, _ = strconv.Atoi(albumResp.ReleaseDate[:4])
	}

	album := &domain.Album{
		ID:          strconv.FormatInt(albumResp.ID, 10),
		ArtistID:    strconv.FormatInt(albumResp.Artist.ID, 10),
		ArtistName:  albumResp.Artist.Name,
		Title:       albumResp.Title,
		ReleaseType: mapRecordType(albumResp.RecordType),
		ReleaseDate: albumResp.ReleaseDate,
		Year:        year,
		CoverURL:    cover,
		TotalTracks: albumResp.NbTracks,
		UPC:         albumResp.UPC,
		Source:      "deezer",
		SourceID:    strconv.FormatInt(albumResp.ID, 10),
		Status:      domain.StatusPending,
	}

	// Fetch full tracks list if truncated or missing in main album payload
	tracks := albumResp.Tracks.Data
	if len(tracks) < albumResp.NbTracks {
		fullTracks, err := c.fetchAlbumTracks(ctx, albumID)
		if err == nil {
			tracks = fullTracks
		}
	}

	for i, t := range tracks {
		title := t.Title
		if t.TitleVersion != "" && !strings.Contains(title, t.TitleVersion) {
			title = fmt.Sprintf("%s (%s)", title, t.TitleVersion)
		}

		artistName := t.Artist.Name
		if artistName == "" {
			artistName = albumResp.Artist.Name
		}

		trackPos := t.TrackPosition
		if trackPos <= 0 {
			trackPos = i + 1
		}
		discNum := t.DiskNumber
		if discNum <= 0 {
			discNum = 1
		}

		album.Tracks = append(album.Tracks, domain.Track{
			ID:          strconv.FormatInt(t.ID, 10),
			AlbumID:     album.ID,
			ArtistName:  artistName,
			Title:       title,
			DiscNumber:  discNum,
			TrackNumber: trackPos,
			DurationSec: t.Duration,
			ISRC:        t.ISRC,
			Explicit:    t.Explicit,
			SourceID:    strconv.FormatInt(t.ID, 10),
		})
	}

	if album.TotalTracks == 0 {
		album.TotalTracks = len(album.Tracks)
	}

	return album, nil
}

func (c *Client) fetchAlbumTracks(ctx context.Context, albumID string) ([]trackItemResponse, error) {
	reqURL := fmt.Sprintf("%s/album/%s/tracks?limit=200", c.baseURL, albumID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tracksResp struct {
		Data []trackItemResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tracksResp); err != nil {
		return nil, err
	}

	return tracksResp.Data, nil
}

func mapRecordType(rt string) domain.ReleaseType {
	switch strings.ToLower(rt) {
	case "album":
		return domain.ReleaseTypeAlbum
	case "ep":
		return domain.ReleaseTypeEP
	case "single":
		return domain.ReleaseTypeSingle
	case "compile", "compilation":
		return domain.ReleaseTypeCompilation
	default:
		return domain.ReleaseTypeOther
	}
}

// getJSON performs a GET against the Deezer API and decodes the body into out.
// Deezer reports some errors as HTTP 200 with an "error" object, so callers that
// care check their own payload for emptiness.
func (c *Client) getJSON(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deezer request %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deezer %s returned status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type albumSearchResponse struct {
	Data []struct {
		ID         int64  `json:"id"`
		Title      string `json:"title"`
		CoverXL    string `json:"cover_xl"`
		CoverBig   string `json:"cover_big"`
		NbTracks   int    `json:"nb_tracks"`
		RecordType string `json:"record_type"`
		Explicit   bool   `json:"explicit_lyrics"`
		Artist     struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"data"`
}

// SearchAlbum searches releases (albums, EPs, singles) by free text.
func (c *Client) SearchAlbum(ctx context.Context, query string) ([]domain.Album, error) {
	var res albumSearchResponse
	if err := c.getJSON(ctx, "/search/album?limit=40&q="+url.QueryEscape(query), &res); err != nil {
		return nil, err
	}
	albums := make([]domain.Album, 0, len(res.Data))
	for _, item := range res.Data {
		cover := item.CoverXL
		if cover == "" {
			cover = item.CoverBig
		}
		id := strconv.FormatInt(item.ID, 10)
		albums = append(albums, domain.Album{
			ID:          id,
			ArtistID:    strconv.FormatInt(item.Artist.ID, 10),
			ArtistName:  item.Artist.Name,
			Title:       item.Title,
			ReleaseType: mapRecordType(item.RecordType),
			CoverURL:    cover,
			TotalTracks: item.NbTracks,
			Source:      "deezer",
			SourceID:    id,
			Status:      domain.StatusPending,
		})
	}
	return albums, nil
}

type trackSearchItem struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	ISRC     string `json:"isrc"`
	Duration int    `json:"duration"`
	Artist   struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		CoverXL  string `json:"cover_xl"`
		CoverBig string `json:"cover_big"`
	} `json:"album"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (t trackSearchItem) hit() indexer.TrackHit {
	cover := t.Album.CoverXL
	if cover == "" {
		cover = t.Album.CoverBig
	}
	return indexer.TrackHit{
		TrackID:     strconv.FormatInt(t.ID, 10),
		Title:       t.Title,
		ArtistName:  t.Artist.Name,
		DurationSec: t.Duration,
		ISRC:        t.ISRC,
		AlbumID:     strconv.FormatInt(t.Album.ID, 10),
		AlbumTitle:  t.Album.Title,
		CoverURL:    cover,
	}
}

// SearchTrack searches tracks by artist and title using Deezer's advanced search
// syntax, falling back to a plain query when the strict form finds nothing.
func (c *Client) SearchTrack(ctx context.Context, artist, title string) ([]indexer.TrackHit, error) {
	var queries []string
	if strings.TrimSpace(artist) != "" {
		queries = append(queries, fmt.Sprintf("artist:\"%s\" track:\"%s\"", artist, title))
	}
	queries = append(queries, strings.TrimSpace(artist+" "+title))
	for _, q := range queries {
		var res struct {
			Data []trackSearchItem `json:"data"`
		}
		if err := c.getJSON(ctx, "/search/track?limit=15&q="+url.QueryEscape(q), &res); err != nil {
			return nil, err
		}
		if len(res.Data) == 0 {
			continue
		}
		hits := make([]indexer.TrackHit, 0, len(res.Data))
		for _, t := range res.Data {
			hits = append(hits, t.hit())
		}
		return hits, nil
	}
	return nil, nil
}

// LookupISRC resolves a track by ISRC. Returns nil, nil when Deezer has no match.
func (c *Client) LookupISRC(ctx context.Context, isrc string) (*indexer.TrackHit, error) {
	var t trackSearchItem
	if err := c.getJSON(ctx, "/track/isrc:"+url.PathEscape(isrc), &t); err != nil {
		return nil, err
	}
	if t.Error != nil || t.ID == 0 || t.Album.ID == 0 {
		return nil, nil
	}
	h := t.hit()
	return &h, nil
}

// GetTrack fetches one track by its Deezer ID. Returns nil, nil when not found.
func (c *Client) GetTrack(ctx context.Context, id string) (*indexer.TrackHit, error) {
	var t trackSearchItem
	if err := c.getJSON(ctx, "/track/"+url.PathEscape(id), &t); err != nil {
		return nil, err
	}
	if t.Error != nil || t.ID == 0 || t.Album.ID == 0 {
		return nil, nil
	}
	h := t.hit()
	return &h, nil
}
