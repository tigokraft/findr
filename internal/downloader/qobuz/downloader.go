package qobuz

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
)

const (
	apiBaseURL = "https://www.qobuz.com/api.json/0.2"
)

// Downloader handles downloading music directly from Qobuz in lossless FLAC or Hi-Res.
type Downloader struct {
	appID         string
	appSecret     string
	username      string
	password      string
	userToken     string
	targetQuality domain.AudioQuality
	httpClient    *http.Client
	mu            sync.Mutex
}

// Config holds credentials and parameters for Qobuz.
type Config struct {
	AppID         string
	AppSecret     string
	Username      string
	Password      string
	UserToken     string
	TargetQuality domain.AudioQuality
}

// NewDownloader creates a new Qobuz downloader instance.
func NewDownloader(cfg Config) *Downloader {
	appID := cfg.AppID
	if appID == "" {
		appID = "798273057"
	}
	appSecret := cfg.AppSecret
	if appSecret == "" {
		if appID == "798273057" {
			appSecret = "abb21364945c0583309667d13ca3d93a"
		} else if appID == "305874112" {
			appSecret = "d9f2e3798aa12ff5e13589b14c33f81e"
		}
	}
	quality := cfg.TargetQuality
	if quality == "" {
		quality = domain.QualityFLAC16
	}

	return &Downloader{
		appID:         appID,
		appSecret:     appSecret,
		username:      cfg.Username,
		password:      cfg.Password,
		userToken:     cfg.UserToken,
		targetQuality: quality,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// UserInfo contains details about the authenticated Qobuz user and subscription.
type UserInfo struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	Country      string `json:"country"`
	Subscription string `json:"subscription"`
	IsActive     bool   `json:"is_active"`
}

func (d *Downloader) Name() string {
	return "qobuz"
}

// UserToken returns current active user token.
func (d *Downloader) UserToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.userToken
}

// SetUserToken explicitly sets the user token.
func (d *Downloader) SetUserToken(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.userToken = strings.TrimSpace(token)
}

// ValidateToken verifies that the configured user token is valid against Qobuz API.
func (d *Downloader) ValidateToken(ctx context.Context) (*UserInfo, error) {
	d.mu.Lock()
	token := d.userToken
	d.mu.Unlock()

	if token == "" {
		return nil, fmt.Errorf("no qobuz user token configured")
	}

	reqURL := fmt.Sprintf("%s/user/get?app_id=%s&user_auth_token=%s", apiBaseURL, d.appID, url.QueryEscape(token))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-User-Auth-Token", token)
	req.Header.Set("X-App-Id", d.appID)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz validate token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("invalid qobuz user token (status %d): %s", resp.StatusCode, string(body))
	}

	var userResp struct {
		ID           int64  `json:"id"`
		Email        string `json:"email"`
		CountryCode  string `json:"country_code"`
		Subscription struct {
			Offer  string `json:"offer"`
			Period string `json:"period"`
		} `json:"subscription"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&userResp); err != nil {
		return nil, fmt.Errorf("decode user response: %w", err)
	}

	return &UserInfo{
		ID:           userResp.ID,
		Email:        userResp.Email,
		Country:      userResp.CountryCode,
		Subscription: userResp.Subscription.Offer,
		IsActive:     true,
	}, nil
}

// Authenticate logs in with credentials if user token is not yet established.
func (d *Downloader) Authenticate(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.userToken != "" {
		return nil
	}
	if d.username == "" || d.password == "" {
		return nil
	}

	reqURL := fmt.Sprintf("%s/user/login?username=%s&password=%s&app_id=%s",
		apiBaseURL, url.QueryEscape(d.username), url.QueryEscape(d.password), d.appID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("qobuz login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qobuz login failed with status %d", resp.StatusCode)
	}

	var loginResp struct {
		UserToken string `json:"user_auth_token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}

	d.userToken = loginResp.UserToken
	return nil
}

// Search checks if the requested album exists in Qobuz's catalog.
func (d *Downloader) Search(ctx context.Context, album *domain.Album) ([]downloader.SearchResult, error) {
	query := fmt.Sprintf("%s %s", album.ArtistName, album.Title)
	reqURL := fmt.Sprintf("%s/album/search?query=%s&app_id=%s&limit=5", apiBaseURL, url.QueryEscape(query), d.appID)
	if d.userToken != "" {
		reqURL += fmt.Sprintf("&user_auth_token=%s", url.QueryEscape(d.userToken))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-App-Id", d.appID)
	if d.userToken != "" {
		req.Header.Set("X-User-Auth-Token", d.userToken)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz search request: %w", err)
	}
	defer resp.Body.Close()

	var searchResp struct {
		Albums struct {
			Items []struct {
				ID                  string  `json:"id"`
				Title               string  `json:"title"`
				MaximumBitDepth     int     `json:"maximum_bit_depth"`
				MaximumSamplingRate float64 `json:"maximum_sampling_rate"`
				TracksCount         int     `json:"tracks_count"`
				Artist              struct {
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"items"`
		} `json:"albums"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	var results []downloader.SearchResult
	for _, item := range searchResp.Albums.Items {
		quality := domain.QualityFLAC16
		if item.MaximumBitDepth > 16 || item.MaximumSamplingRate > 44.1 {
			quality = domain.QualityFLAC2496
		}

		results = append(results, downloader.SearchResult{
			ID:          item.ID,
			Title:       item.Title,
			Artist:      item.Artist.Name,
			Format:      quality,
			SampleRate:  int(item.MaximumSamplingRate * 1000),
			BitDepth:    item.MaximumBitDepth,
			TotalTracks: item.TracksCount,
			Source:      "qobuz",
		})
	}

	return results, nil
}

type qobuzAlbumMatch struct {
	trackByNum   map[int]string
	trackByTitle map[string]string
	tracks       []string
}

// resolveQobuzTracks maps external indexer album tracks to native Qobuz track IDs.
func (d *Downloader) resolveQobuzTracks(ctx context.Context, album *domain.Album) (*qobuzAlbumMatch, error) {
	query := fmt.Sprintf("%s %s", album.ArtistName, album.Title)
	reqURL := fmt.Sprintf("%s/album/search?query=%s&app_id=%s&limit=8", apiBaseURL, url.QueryEscape(query), d.appID)
	if d.userToken != "" {
		reqURL += fmt.Sprintf("&user_auth_token=%s", url.QueryEscape(d.userToken))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-App-Id", d.appID)
	if d.userToken != "" {
		req.Header.Set("X-User-Auth-Token", d.userToken)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qobuz search failed: %w", err)
	}
	defer resp.Body.Close()

	var searchResp struct {
		Albums struct {
			Items []struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				UPC         string `json:"upc"`
				TracksCount int    `json:"tracks_count"`
			} `json:"items"`
		} `json:"albums"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("decode qobuz search response: %w", err)
	}

	items := searchResp.Albums.Items
	if len(items) == 0 {
		return nil, fmt.Errorf("album '%s - %s' not found on Qobuz catalog", album.ArtistName, album.Title)
	}

	matchedAlbumID := items[0].ID
	if album.UPC != "" {
		for _, it := range items {
			if it.UPC == album.UPC || strings.TrimLeft(it.UPC, "0") == strings.TrimLeft(album.UPC, "0") {
				matchedAlbumID = it.ID
				break
			}
		}
	}

	albumURL := fmt.Sprintf("%s/album/get?album_id=%s&app_id=%s", apiBaseURL, matchedAlbumID, d.appID)
	if d.userToken != "" {
		albumURL += fmt.Sprintf("&user_auth_token=%s", url.QueryEscape(d.userToken))
	}

	reqAlb, err := http.NewRequestWithContext(ctx, http.MethodGet, albumURL, nil)
	if err != nil {
		return nil, err
	}
	reqAlb.Header.Set("X-App-Id", d.appID)
	if d.userToken != "" {
		reqAlb.Header.Set("X-User-Auth-Token", d.userToken)
	}

	respAlb, err := d.httpClient.Do(reqAlb)
	if err != nil {
		return nil, fmt.Errorf("qobuz get album tracks failed: %w", err)
	}
	defer respAlb.Body.Close()

	var albResp struct {
		Tracks struct {
			Items []struct {
				ID          int64  `json:"id"`
				Title       string `json:"title"`
				TrackNumber int    `json:"track_number"`
				MediaNumber int    `json:"media_number"`
			} `json:"items"`
		} `json:"tracks"`
	}

	if err := json.NewDecoder(respAlb.Body).Decode(&albResp); err != nil {
		return nil, fmt.Errorf("decode qobuz album details: %w", err)
	}

	match := &qobuzAlbumMatch{
		trackByNum:   make(map[int]string),
		trackByTitle: make(map[string]string),
		tracks:       make([]string, 0, len(albResp.Tracks.Items)),
	}
	for _, t := range albResp.Tracks.Items {
		trackID := strconv.FormatInt(t.ID, 10)
		match.trackByNum[t.TrackNumber] = trackID
		match.trackByTitle[strings.ToLower(strings.TrimSpace(t.Title))] = trackID
		match.tracks = append(match.tracks, trackID)
	}

	return match, nil
}

// Download downloads all tracks of the specified album into targetDir.
func (d *Downloader) Download(ctx context.Context, album *domain.Album, targetDir string, progress downloader.ProgressFunc) (*domain.DownloadJob, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("create target dir: %w", err)
	}

	if err := d.Authenticate(ctx); err != nil {
		return nil, fmt.Errorf("authenticate qobuz: %w", err)
	}

	job := &domain.DownloadJob{
		ID:            fmt.Sprintf("qobuz-%s-%d", album.ID, time.Now().Unix()),
		AlbumID:       album.ID,
		Downloader:    "qobuz",
		DownloadDir:   targetDir,
		TargetQuality: d.targetQuality,
		Status:        domain.StatusDownloading,
		Progress:      0.0,
		CreatedAt:     time.Now(),
	}

	formatID := d.mapQualityToFormatID(d.targetQuality)
	totalTracks := len(album.Tracks)
	if totalTracks == 0 {
		return nil, fmt.Errorf("album has no tracks defined")
	}

	fmt.Printf("[Qobuz Downloader] Preparing download for '%s - %s' (Source: %s, Tracks: %d)\n",
		album.ArtistName, album.Title, album.Source, totalTracks)

	// Resolve external track IDs (e.g. from Deezer) to native Qobuz track IDs
	var qobuzMatch *qobuzAlbumMatch
	if album.Source != "qobuz" {
		fmt.Printf("[Qobuz Downloader] Resolving external '%s' release on Qobuz catalog...\n", album.Source)
		resolved, err := d.resolveQobuzTracks(ctx, album)
		if err != nil {
			fmt.Printf("[Qobuz Downloader] Warning: could not resolve Qobuz album: %v\n", err)
			return nil, fmt.Errorf("resolve album on qobuz: %w", err)
		}
		qobuzMatch = resolved
		fmt.Printf("[Qobuz Downloader] Successfully matched %d tracks on Qobuz\n", len(qobuzMatch.tracks))
	}

	var downloadedFiles []string
	for i, trk := range album.Tracks {
		select {
		case <-ctx.Done():
			job.Status = domain.StatusFailed
			job.ErrorMessage = "download cancelled"
			return job, ctx.Err()
		default:
		}

		fileExt := "flac"
		if formatID == 5 {
			fileExt = "mp3"
		}

		trackNum := trk.TrackNumber
		if trackNum <= 0 {
			trackNum = i + 1
		}

		sanitizedTitle := sanitizeFilename(trk.Title)
		filename := fmt.Sprintf("%02d - %s.%s", trackNum, sanitizedTitle, fileExt)
		if trk.DiscNumber > 1 {
			filename = fmt.Sprintf("%d-%02d - %s.%s", trk.DiscNumber, trackNum, sanitizedTitle, fileExt)
		}
		destPath := filepath.Join(targetDir, filename)

		if progress != nil {
			pct := float64(i) / float64(totalTracks) * 100.0
			progress(pct, fmt.Sprintf("Downloading track [%d/%d]: %s", i+1, totalTracks, trk.Title))
		}

		trackID := trk.SourceID
		if qobuzMatch != nil {
			if qID, ok := qobuzMatch.trackByNum[trackNum]; ok && qID != "" {
				trackID = qID
			} else if qID, ok := qobuzMatch.trackByNum[i+1]; ok && qID != "" {
				trackID = qID
			} else if qID, ok := qobuzMatch.trackByTitle[strings.ToLower(strings.TrimSpace(trk.Title))]; ok && qID != "" {
				trackID = qID
			} else if i < len(qobuzMatch.tracks) {
				trackID = qobuzMatch.tracks[i]
			} else {
				return nil, fmt.Errorf("track %d '%s' not found on Qobuz album", trackNum, trk.Title)
			}
		}

		fmt.Printf("[Qobuz Downloader] Downloading track [%d/%d] (Qobuz Track ID: %s): %s\n", i+1, totalTracks, trackID, trk.Title)

		fileURL, err := d.getTrackFileURL(ctx, trackID, formatID)
		if err != nil {
			return nil, fmt.Errorf("get file url for track %d '%s' (qobuz_id=%s): %w", trackNum, trk.Title, trackID, err)
		}

		if err := d.downloadToFile(ctx, fileURL, destPath); err != nil {
			return nil, fmt.Errorf("download track %s to %s: %w", trk.Title, destPath, err)
		}

		downloadedFiles = append(downloadedFiles, destPath)
	}

	now := time.Now()
	job.Status = domain.StatusDownloaded
	job.Progress = 100.0
	job.DownloadedFiles = downloadedFiles
	job.CompletedAt = &now

	if progress != nil {
		progress(100.0, "All tracks downloaded successfully")
	}

	fmt.Printf("[Qobuz Downloader] Successfully downloaded all %d tracks for '%s - %s'\n",
		len(downloadedFiles), album.ArtistName, album.Title)

	return job, nil
}

// CalculateSignature generates MD5 signature for Qobuz track/getFileUrl.
// Signature = md5("trackgetFileUrl" + sorted_params + request_ts + app_secret)
func CalculateSignature(params map[string]string, requestTS int64, appSecret string) string {
	var keys []string
	for k := range params {
		if k == "app_id" || k == "user_auth_token" || k == "request_sig" || k == "request_ts" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("trackgetFileUrl")
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString(params[k])
	}
	sb.WriteString(strconv.FormatInt(requestTS, 10))
	sb.WriteString(appSecret)

	hash := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(hash[:])
}

func (d *Downloader) getTrackFileURL(ctx context.Context, trackID string, formatID int) (string, error) {
	ts := time.Now().Unix()
	params := map[string]string{
		"track_id":  trackID,
		"format_id": strconv.Itoa(formatID),
		"intent":    "stream",
	}

	var sig string
	if d.appSecret != "" {
		sig = CalculateSignature(params, ts, d.appSecret)
	}

	reqURL := fmt.Sprintf("%s/track/getFileUrl?track_id=%s&format_id=%d&intent=stream&request_ts=%d&app_id=%s",
		apiBaseURL, trackID, formatID, ts, d.appID)

	if sig != "" {
		reqURL += fmt.Sprintf("&request_sig=%s", sig)
	}
	if d.userToken != "" {
		reqURL += fmt.Sprintf("&user_auth_token=%s", url.QueryEscape(d.userToken))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}

	if d.userToken != "" {
		req.Header.Set("X-User-Auth-Token", d.userToken)
	}
	req.Header.Set("X-App-Id", d.appID)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("qobuz getFileUrl request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("qobuz getFileUrl status %d: %s", resp.StatusCode, string(body))
	}

	var fileResp struct {
		URL string `json:"url"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil {
		return "", fmt.Errorf("decode file url response: %w", err)
	}

	if fileResp.URL == "" {
		return "", fmt.Errorf("no media url returned for track %s", trackID)
	}

	return fileResp.URL, nil
}

func (d *Downloader) downloadToFile(ctx context.Context, mediaURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return err
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("media download returned status %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func (d *Downloader) mapQualityToFormatID(quality domain.AudioQuality) int {
	switch quality {
	case domain.QualityFLAC2419:
		return 27 // 24-bit / 192kHz FLAC
	case domain.QualityFLAC2496:
		return 7 // 24-bit / 96kHz FLAC
	case domain.QualityMP3320:
		return 5 // MP3 320kbps
	default:
		return 6 // 16-bit / 44.1kHz FLAC CD Quality
	}
}

func sanitizeFilename(name string) string {
	invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	res := name
	for _, c := range invalidChars {
		res = strings.ReplaceAll(res, c, "_")
	}
	return strings.TrimSpace(res)
}
