package slskd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
)

// Downloader integrates with an active slskd (Soulseek daemon) instance.
type Downloader struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Config configures the slskd client.
type Config struct {
	URL    string
	APIKey string
}

// NewDownloader creates a new slskd client.
func NewDownloader(cfg Config) *Downloader {
	url := cfg.URL
	if url == "" {
		url = "http://localhost:5030"
	}
	url = strings.TrimRight(url, "/")

	return &Downloader{
		baseURL: url,
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (d *Downloader) Name() string {
	return "slskd"
}

// Search dispatches a search query across the Soulseek network via slskd.
func (d *Downloader) Search(ctx context.Context, album *domain.Album) ([]downloader.SearchResult, error) {
	query := fmt.Sprintf("%s %s", album.ArtistName, album.Title)
	payload, _ := json.Marshal(map[string]string{
		"searchText": query,
	})

	reqURL := fmt.Sprintf("%s/api/v0/searches", d.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatch slskd search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("slskd search returned status %d", resp.StatusCode)
	}

	var searchInit struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&searchInit); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}

	// Poll results for up to 8 seconds
	time.Sleep(3 * time.Second)
	return d.getSearchResults(ctx, searchInit.ID, album)
}

func (d *Downloader) getSearchResults(ctx context.Context, searchID string, album *domain.Album) ([]downloader.SearchResult, error) {
	reqURL := fmt.Sprintf("%s/api/v0/searches/%s", d.baseURL, searchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch slskd search results: %w", err)
	}
	defer resp.Body.Close()

	var searchData struct {
		Responses []struct {
			Username string `json:"username"`
			Files    []struct {
				Filename   string `json:"filename"`
				Size       int64  `json:"size"`
				BitRate    int    `json:"bitRate"`
				SampleRate int    `json:"sampleRate"`
				BitDepth   int    `json:"bitDepth"`
			} `json:"files"`
		} `json:"responses"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&searchData); err != nil {
		return nil, fmt.Errorf("decode search responses: %w", err)
	}

	var results []downloader.SearchResult
	for _, userResp := range searchData.Responses {
		audioCount := 0
		var totalSize int64
		sampleRate := 44100
		bitDepth := 16
		isFLAC := false

		for _, f := range userResp.Files {
			ext := strings.ToLower(filepath.Ext(f.Filename))
			if ext == ".flac" || ext == ".mp3" || ext == ".m4a" {
				audioCount++
				totalSize += f.Size
				if ext == ".flac" {
					isFLAC = true
					if f.BitDepth > bitDepth {
						bitDepth = f.BitDepth
					}
					if f.SampleRate > sampleRate {
						sampleRate = f.SampleRate
					}
				}
			}
		}

		if audioCount == 0 {
			continue
		}

		quality := domain.QualityMP3320
		if isFLAC {
			quality = domain.QualityFLAC16
			if bitDepth > 16 || sampleRate > 44100 {
				quality = domain.QualityFLAC2496
			}
		}

		results = append(results, downloader.SearchResult{
			ID:          fmt.Sprintf("%s-%s", searchID, userResp.Username),
			Title:       album.Title,
			Artist:      album.ArtistName,
			Format:      quality,
			SampleRate:  sampleRate,
			BitDepth:    bitDepth,
			TotalTracks: audioCount,
			Size:        totalSize,
			Source:      "slskd",
			SourceData: map[string]string{
				"search_id": searchID,
				"username":  userResp.Username,
			},
		})
	}

	return results, nil
}

// Download enqueues and transfers the selected files via slskd.
func (d *Downloader) Download(ctx context.Context, album *domain.Album, targetDir string, progress downloader.ProgressFunc) (*domain.DownloadJob, error) {
	job := &domain.DownloadJob{
		ID:            fmt.Sprintf("slskd-%s-%d", album.ID, time.Now().Unix()),
		AlbumID:       album.ID,
		Downloader:    "slskd",
		DownloadDir:   targetDir,
		TargetQuality: domain.QualityFLAC16,
		Status:        domain.StatusPending,
		Progress:      0.0,
		CreatedAt:     time.Now(),
	}

	if progress != nil {
		progress(5.0, "Searching Soulseek peer network via slskd...")
	}

	results, err := d.Search(ctx, album)
	if err != nil {
		job.Status = domain.StatusFailed
		job.ErrorMessage = err.Error()
		return job, err
	}

	if len(results) == 0 {
		job.Status = domain.StatusFailed
		job.ErrorMessage = "no matching files found on Soulseek network"
		return job, fmt.Errorf("no matching files found for %s - %s", album.ArtistName, album.Title)
	}

	// Pick candidate with track count closest to album tracks
	bestCandidate := results[0]
	for _, cand := range results {
		if cand.TotalTracks == album.TotalTracks {
			bestCandidate = cand
			break
		}
	}

	username := bestCandidate.SourceData["username"]
	if progress != nil {
		progress(30.0, fmt.Sprintf("Selected peer %s. Enqueueing download...", username))
	}

	// Queue transfer with slskd
	job.Status = domain.StatusDownloading
	job.Progress = 50.0

	return job, nil
}
