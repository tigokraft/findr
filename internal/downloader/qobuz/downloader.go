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
		appID = "305874112"
	}
	quality := cfg.TargetQuality
	if quality == "" {
		quality = domain.QualityFLAC16
	}

	return &Downloader{
		appID:         appID,
		appSecret:     cfg.AppSecret,
		username:      cfg.Username,
		password:      cfg.Password,
		userToken:     cfg.UserToken,
		targetQuality: quality,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (d *Downloader) Name() string {
	return "qobuz"
}

// Authenticate logs in with credentials if user token is not yet established.
func (d *Downloader) Authenticate(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.userToken != "" {
		return nil
	}
	if d.username == "" || d.password == "" {
		return nil // Allow unauthenticated calls if running in preview/sample mode
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
		User      struct {
			ID           int64  `json:"id"`
			CredentialID int64  `json:"credential_id"`
			Email        string `json:"email"`
		} `json:"user"`
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
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

		sanitizedTitle := sanitizeFilename(trk.Title)
		filename := fmt.Sprintf("%02d - %s.%s", trk.TrackNumber, sanitizedTitle, fileExt)
		if trk.DiscNumber > 1 {
			filename = fmt.Sprintf("%d-%02d - %s.%s", trk.DiscNumber, trk.TrackNumber, sanitizedTitle, fileExt)
		}
		destPath := filepath.Join(targetDir, filename)

		if progress != nil {
			pct := float64(i) / float64(totalTracks) * 100.0
			progress(pct, fmt.Sprintf("Downloading track %d/%d: %s", i+1, totalTracks, trk.Title))
		}

		fileURL, err := d.getTrackFileURL(ctx, trk.SourceID, formatID)
		if err != nil {
			return nil, fmt.Errorf("get file url for track %s (%s): %w", trk.Title, trk.SourceID, err)
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

func (d *Downloader) mapQualityToFormatID(q domain.AudioQuality) int {
	switch q {
	case domain.QualityMP3320:
		return 5
	case domain.QualityFLAC16:
		return 6
	case domain.QualityFLAC2496:
		return 7
	case domain.QualityFLAC2419:
		return 27
	default:
		return 6
	}
}

func sanitizeFilename(s string) string {
	invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	res := s
	for _, c := range invalidChars {
		res = strings.ReplaceAll(res, c, "_")
	}
	return strings.TrimSpace(res)
}
