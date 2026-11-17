package slskd

import (
	"bytes"
	"context"
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
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
)

// Downloader integrates with an active slskd (Soulseek daemon) instance.
type Downloader struct {
	baseURL     string
	apiKey      string
	downloadDir string
	httpClient  *http.Client
}

// Config configures the slskd client.
type Config struct {
	URL         string
	APIKey      string
	DownloadDir string
}

// FilePayload matches slskd's transfer enqueue format: [{"filename": ..., "size": ...}]
type FilePayload struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

// SlskdResponseFile represents an individual file in slskd's search response.
type SlskdResponseFile struct {
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	BitRate    int    `json:"bitRate"`
	SampleRate int    `json:"sampleRate"`
	BitDepth   int    `json:"bitDepth"`
	IsLocked   bool   `json:"isLocked"`
}

// SlskdSearchResponse represents one peer's response in slskd's search.
type SlskdSearchResponse struct {
	Username          string              `json:"username"`
	UploadSpeed       int64               `json:"uploadSpeed"`
	QueueLength       int                 `json:"queueLength"`
	HasFreeUploadSlot bool                `json:"hasFreeUploadSlot"`
	FileCount         int                 `json:"fileCount"`
	Files             []SlskdResponseFile `json:"files"`
}

// SlskdTransferFile represents a file in slskd's transfers response.
type SlskdTransferFile struct {
	ID               string  `json:"id"`
	Username         string  `json:"username"`
	Filename         string  `json:"filename"`
	Size             int64   `json:"size"`
	State            string  `json:"state"`
	BytesTransferred int64   `json:"bytesTransferred"`
	AverageSpeed     float64 `json:"averageSpeed"`
	PercentComplete  int     `json:"percentComplete"`
}

// SlskdDirectoryTransfers represents directory-grouped transfers.
type SlskdDirectoryTransfers struct {
	Directory string              `json:"directory"`
	FileCount int                 `json:"fileCount"`
	Files     []SlskdTransferFile `json:"files"`
}

// SlskdUserTransfers represents transfers for a username.
type SlskdUserTransfers struct {
	Username    string                    `json:"username"`
	Directories []SlskdDirectoryTransfers `json:"directories"`
}

// NewDownloader creates a new slskd client.
// maxPeerAttempts caps how many Soulseek peers are tried for one album or track.
const maxPeerAttempts = 5

func NewDownloader(cfg Config) *Downloader {
	u := cfg.URL
	if u == "" {
		u = "http://localhost:5030"
	}
	u = strings.TrimRight(u, "/")

	dlDir := cfg.DownloadDir
	if dlDir == "" {
		dlDir = "/downloads"
	}

	return &Downloader{
		baseURL:     u,
		apiKey:      cfg.APIKey,
		downloadDir: dlDir,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

func (d *Downloader) Name() string {
	return "slskd"
}

// Ping verifies connectivity and authentication with slskd.
func (d *Downloader) Ping(ctx context.Context) error {
	reqURL := fmt.Sprintf("%s/api/v0/application", d.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to slskd: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slskd returned status %d", resp.StatusCode)
	}
	return nil
}

// Search dispatches a search query across the Soulseek network via slskd.
func (d *Downloader) Search(ctx context.Context, album *domain.Album) ([]downloader.SearchResult, error) {
	query := fmt.Sprintf("%s %s", album.ArtistName, album.Title)
	payload, _ := json.Marshal(map[string]interface{}{
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

	searchID := searchInit.ID
	defer d.deleteSearch(context.Background(), searchID)

	// Poll search state until completed or sufficient results are available
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timeout := time.After(10 * time.Second)

	var lastResults []downloader.SearchResult
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return lastResults, nil
		case <-ticker.C:
			res, completed, err := d.fetchSearchResults(ctx, searchID, album)
			if err == nil && len(res) > 0 {
				lastResults = res
				if completed || len(res) >= 10 {
					return res, nil
				}
			} else if completed {
				return lastResults, nil
			}
		}
	}
}

func (d *Downloader) fetchSearchResults(ctx context.Context, searchID string, album *domain.Album) ([]downloader.SearchResult, bool, error) {
	// 1. Check state
	stateURL := fmt.Sprintf("%s/api/v0/searches/%s", d.baseURL, searchID)
	sReq, err := http.NewRequestWithContext(ctx, http.MethodGet, stateURL, nil)
	if err != nil {
		return nil, false, err
	}
	if d.apiKey != "" {
		sReq.Header.Set("X-API-Key", d.apiKey)
	}

	sResp, err := d.httpClient.Do(sReq)
	if err != nil {
		return nil, false, err
	}
	defer sResp.Body.Close()

	var searchState struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(sResp.Body).Decode(&searchState)
	isCompleted := searchState.State != "" && searchState.State != "InProgress"

	// 2. Fetch responses
	respURL := fmt.Sprintf("%s/api/v0/searches/%s/responses", d.baseURL, searchID)
	rReq, err := http.NewRequestWithContext(ctx, http.MethodGet, respURL, nil)
	if err != nil {
		return nil, isCompleted, err
	}
	if d.apiKey != "" {
		rReq.Header.Set("X-API-Key", d.apiKey)
	}

	rResp, err := d.httpClient.Do(rReq)
	if err != nil {
		return nil, isCompleted, err
	}
	defer rResp.Body.Close()

	var userResponses []SlskdSearchResponse
	if err := json.NewDecoder(rResp.Body).Decode(&userResponses); err != nil {
		return nil, isCompleted, err
	}

	var results []downloader.SearchResult
	for _, userResp := range userResponses {
		// Group files by parent directory
		dirMap := make(map[string][]SlskdResponseFile)
		for _, f := range userResp.Files {
			if f.IsLocked {
				continue
			}
			norm := strings.ReplaceAll(f.Filename, "\\", "/")
			ext := strings.ToLower(filepath.Ext(norm))
			if !isAudioExtension(ext) {
				continue
			}
			dir := filepath.Dir(norm)
			dirMap[dir] = append(dirMap[dir], f)
		}

		for dirPath, dirFiles := range dirMap {
			if len(dirFiles) == 0 {
				continue
			}

			var totalSize int64
			sampleRate := 44100
			bitDepth := 16
			bitRate := 0
			isFLAC := false

			payloadFiles := make([]FilePayload, 0, len(dirFiles))
			for _, f := range dirFiles {
				norm := strings.ReplaceAll(f.Filename, "\\", "/")
				ext := strings.ToLower(filepath.Ext(norm))
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
				if f.BitRate > bitRate {
					bitRate = f.BitRate
				}
				payloadFiles = append(payloadFiles, FilePayload{
					Filename: f.Filename,
					Size:     f.Size,
				})
			}

			quality := domain.QualityMP3320
			if isFLAC {
				quality = domain.QualityFLAC16
				if bitDepth > 16 || sampleRate > 44100 {
					quality = domain.QualityFLAC2496
				}
			}

			subDir := filepath.Base(dirPath)
			filesJSON, _ := json.Marshal(payloadFiles)

			freeSlotScore := "0"
			if userResp.HasFreeUploadSlot {
				freeSlotScore = "1"
			}

			results = append(results, downloader.SearchResult{
				ID:          fmt.Sprintf("slskd-%s-%s", userResp.Username, subDir),
				Title:       album.Title,
				Artist:      album.ArtistName,
				Format:      quality,
				SampleRate:  sampleRate,
				BitDepth:    bitDepth,
				Bitrate:     bitRate,
				TotalTracks: len(dirFiles),
				Size:        totalSize,
				Source:      "slskd",
				SourceData: map[string]string{
					"search_id":   searchID,
					"username":    userResp.Username,
					"folder_name": subDir,
					"free_slot":   freeSlotScore,
					"queue_len":   fmt.Sprintf("%d", userResp.QueueLength),
					"files_json":  string(filesJSON),
				},
			})
		}
	}

	// Sort results:
	// 1. Exact match on track count if album.TotalTracks > 0
	// 2. Format: FLAC 24/96 > FLAC 16/44 > MP3 320
	// 3. Free upload slot
	// 4. Shorter queue length
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if album.TotalTracks > 0 {
			aExact := a.TotalTracks == album.TotalTracks
			bExact := b.TotalTracks == album.TotalTracks
			if aExact != bExact {
				return aExact
			}
		}
		if a.Format != b.Format {
			return a.Format == domain.QualityFLAC2496 || (a.Format == domain.QualityFLAC16 && b.Format != domain.QualityFLAC2496)
		}
		if a.SourceData["free_slot"] != b.SourceData["free_slot"] {
			return a.SourceData["free_slot"] == "1"
		}
		aq, _ := strconv.Atoi(a.SourceData["queue_len"])
		bq, _ := strconv.Atoi(b.SourceData["queue_len"])
		return aq < bq
	})

	return results, isCompleted, nil
}

func (d *Downloader) deleteSearch(ctx context.Context, searchID string) {
	if searchID == "" {
		return
	}
	delURL := fmt.Sprintf("%s/api/v0/searches/%s", d.baseURL, searchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
	if err != nil {
		return
	}
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}
	resp, err := d.httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// Download enqueues and transfers the selected album via Soulseek, placing completed files into targetDir.
func (d *Downloader) Download(ctx context.Context, album *domain.Album, targetDir string, progress downloader.ProgressFunc) (*domain.DownloadJob, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}

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
		progress(5.0, fmt.Sprintf("Searching Soulseek network for '%s - %s'...", album.ArtistName, album.Title))
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
		return job, fmt.Errorf("no matching files found on Soulseek network for %s - %s", album.ArtistName, album.Title)
	}

	// Try candidates in ranked order. Peers often go offline or stop answering
	// between search and enqueue (slskd reports this as a 500 "wait timed out"),
	// so one bad peer must not fail the whole album.
	var downloadedFiles []string
	var attemptErrs []string
	attempts := 0
	for _, candidate := range results {
		if attempts >= maxPeerAttempts {
			break
		}
		if ctx.Err() != nil {
			job.Status = domain.StatusFailed
			job.ErrorMessage = "download cancelled"
			return job, ctx.Err()
		}

		username := candidate.SourceData["username"]
		folderName := candidate.SourceData["folder_name"]
		var files []FilePayload
		if err := json.Unmarshal([]byte(candidate.SourceData["files_json"]), &files); err != nil || len(files) == 0 {
			continue
		}
		attempts++

		job.TargetQuality = candidate.Format
		job.Status = domain.StatusDownloading
		if progress != nil {
			progress(10.0, fmt.Sprintf("Selected peer '%s' (%d tracks, %s). Enqueueing download...", username, len(files), candidate.Format))
		}

		if err := d.enqueueFiles(ctx, username, files); err != nil {
			fmt.Printf("[slskd] Enqueue from peer %s failed: %v. Trying next peer...\n", username, err)
			attemptErrs = append(attemptErrs, fmt.Sprintf("%s: enqueue: %v", username, err))
			continue
		}

		if err := d.waitForDownload(ctx, username, files, progress); err != nil {
			if ctx.Err() != nil {
				job.Status = domain.StatusFailed
				job.ErrorMessage = err.Error()
				return job, err
			}
			d.cancelUserDownloads(context.Background(), username, files)
			fmt.Printf("[slskd] Transfer from peer %s failed: %v. Trying next peer...\n", username, err)
			attemptErrs = append(attemptErrs, fmt.Sprintf("%s: %v", username, err))
			continue
		}

		if progress != nil {
			progress(90.0, "Transfers complete. Relocating files into staging directory...")
		}

		collected, err := d.collectDownloadedFiles(username, folderName, files, targetDir)
		if err != nil {
			attemptErrs = append(attemptErrs, fmt.Sprintf("%s: collect: %v", username, err))
			continue
		}
		downloadedFiles = collected
		break
	}

	if downloadedFiles == nil {
		job.Status = domain.StatusFailed
		if len(attemptErrs) == 0 {
			job.ErrorMessage = "invalid candidate files payload"
			return job, fmt.Errorf("no usable Soulseek candidates for %s - %s", album.ArtistName, album.Title)
		}
		job.ErrorMessage = fmt.Sprintf("all %d Soulseek peers failed: %s", len(attemptErrs), strings.Join(attemptErrs, "; "))
		return job, fmt.Errorf("enqueue download: %s", job.ErrorMessage)
	}

	job.Status = domain.StatusDownloaded
	job.Progress = 100.0
	job.DownloadedFiles = downloadedFiles

	if progress != nil {
		progress(100.0, fmt.Sprintf("Successfully downloaded %d tracks via Soulseek", len(downloadedFiles)))
	}

	return job, nil
}

// DownloadTrack searches Soulseek for an individual track and downloads it to destPath.
// Returns the actual path on disk where the file was saved.
func (d *Downloader) DownloadTrack(ctx context.Context, artist string, track domain.Track, destPath string, progress downloader.ProgressFunc) (string, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", fmt.Errorf("create destination directory: %w", err)
	}

	searchQuery := fmt.Sprintf("%s %s", artist, track.Title)
	if progress != nil {
		progress(5.0, fmt.Sprintf("Searching Soulseek for track: %s - %s...", artist, track.Title))
	}

	payload, err := json.Marshal(map[string]interface{}{
		"searchText": searchQuery,
	})
	if err != nil {
		return "", err
	}

	reqURL := fmt.Sprintf("%s/api/v0/searches", d.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("dispatch slskd track search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("slskd track search returned status %d", resp.StatusCode)
	}

	var searchInit struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&searchInit); err != nil {
		return "", fmt.Errorf("decode track search response: %w", err)
	}

	searchID := searchInit.ID
	defer d.deleteSearch(context.Background(), searchID)

	// Poll for search responses
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timeout := time.After(12 * time.Second)

	type TrackCandidate struct {
		Username string
		File     SlskdResponseFile
		Score    int
	}

	candidates := make(map[string]*TrackCandidate)
	cleanTitle := strings.ToLower(strings.TrimSpace(track.Title))
	cleanArtist := strings.ToLower(strings.TrimSpace(artist))

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timeout:
			goto SelectCandidate
		case <-ticker.C:
			respURL := fmt.Sprintf("%s/api/v0/searches/%s/responses", d.baseURL, searchID)
			rReq, err := http.NewRequestWithContext(ctx, http.MethodGet, respURL, nil)
			if err != nil {
				continue
			}
			if d.apiKey != "" {
				rReq.Header.Set("X-API-Key", d.apiKey)
			}
			rResp, err := d.httpClient.Do(rReq)
			if err != nil {
				continue
			}

			var userResponses []SlskdSearchResponse
			_ = json.NewDecoder(rResp.Body).Decode(&userResponses)
			rResp.Body.Close()

			for _, u := range userResponses {
				for _, f := range u.Files {
					if f.IsLocked {
						continue
					}
					norm := strings.ReplaceAll(f.Filename, "\\", "/")
					ext := strings.ToLower(filepath.Ext(norm))
					if !isAudioExtension(ext) {
						continue
					}

					base := strings.ToLower(filepath.Base(norm))
					lowerNorm := strings.ToLower(norm)

					// File must match track title
					if !strings.Contains(base, cleanTitle) && !strings.Contains(lowerNorm, cleanTitle) {
						continue
					}

					score := 10
					if ext == ".flac" {
						score += 50
					} else if ext == ".mp3" && f.BitRate >= 320 {
						score += 30
					} else if ext == ".mp3" {
						score += 15
					}

					if u.HasFreeUploadSlot {
						score += 25
					}
					if u.QueueLength == 0 {
						score += 25
					} else if u.QueueLength < 5 {
						score += 10
					}

					if strings.Contains(lowerNorm, cleanArtist) {
						score += 20
					}

					key := u.Username + "\x00" + f.Filename
					if prev, ok := candidates[key]; !ok || score > prev.Score {
						candidates[key] = &TrackCandidate{
							Username: u.Username,
							File:     f,
							Score:    score,
						}
					}
				}
			}

			// Once a few strong candidates (FLAC, free slot, empty queue) exist, stop waiting
			strong := 0
			for _, c := range candidates {
				if c.Score >= 100 {
					strong++
				}
			}
			if strong >= maxPeerAttempts {
				goto SelectCandidate
			}
		}
	}

SelectCandidate:
	if len(candidates) == 0 {
		return "", fmt.Errorf("no matching track found on Soulseek network for '%s - %s'", artist, track.Title)
	}

	ranked := make([]*TrackCandidate, 0, len(candidates))
	for _, c := range candidates {
		ranked = append(ranked, c)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Username < ranked[j].Username
	})

	var foundPath string
	var attemptErrs []string
	triedUsers := make(map[string]bool)
	for _, cand := range ranked {
		if len(attemptErrs) >= maxPeerAttempts {
			break
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// One attempt per peer: a peer that timed out will time out again.
		if triedUsers[cand.Username] {
			continue
		}
		triedUsers[cand.Username] = true

		filePayload := []FilePayload{{
			Filename: cand.File.Filename,
			Size:     cand.File.Size,
		}}

		if progress != nil {
			progress(20.0, fmt.Sprintf("Downloading track '%s' from Soulseek peer %s...", track.Title, cand.Username))
		}

		if err := d.enqueueFiles(ctx, cand.Username, filePayload); err != nil {
			fmt.Printf("[slskd] Track enqueue from peer %s failed: %v. Trying next peer...\n", cand.Username, err)
			attemptErrs = append(attemptErrs, fmt.Sprintf("%s: enqueue: %v", cand.Username, err))
			continue
		}

		if err := d.waitForDownload(ctx, cand.Username, filePayload, progress); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			d.cancelUserDownloads(context.Background(), cand.Username, filePayload)
			attemptErrs = append(attemptErrs, fmt.Sprintf("%s: %v", cand.Username, err))
			continue
		}

		// Locate downloaded file in slskd download directory
		baseName := filepath.Base(strings.ReplaceAll(cand.File.Filename, "\\", "/"))
		_ = filepath.Walk(d.downloadDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && filepath.Base(path) == baseName {
				foundPath = path
				return filepath.SkipAll
			}
			return nil
		})
		if foundPath == "" {
			attemptErrs = append(attemptErrs, fmt.Sprintf("%s: downloaded file '%s' not found in %s", cand.Username, baseName, d.downloadDir))
			continue
		}
		break
	}

	if foundPath == "" {
		return "", fmt.Errorf("enqueue track download: all %d Soulseek peers failed: %s", len(attemptErrs), strings.Join(attemptErrs, "; "))
	}

	actualExt := strings.ToLower(filepath.Ext(foundPath))
	finalDestPath := destPath
	if actualExt != "" && strings.ToLower(filepath.Ext(finalDestPath)) != actualExt {
		finalDestPath = strings.TrimSuffix(finalDestPath, filepath.Ext(finalDestPath)) + actualExt
	}

	if err := os.Rename(foundPath, finalDestPath); err != nil {
		if err := copyFile(foundPath, finalDestPath); err != nil {
			return "", fmt.Errorf("move track file from %s to %s: %w", foundPath, finalDestPath, err)
		}
		_ = os.Remove(foundPath)
	}

	if progress != nil {
		progress(100.0, fmt.Sprintf("Successfully downloaded track '%s' via Soulseek", track.Title))
	}

	return finalDestPath, nil
}

func (d *Downloader) enqueueFiles(ctx context.Context, username string, files []FilePayload) error {
	payload, err := json.Marshal(files)
	if err != nil {
		return err
	}

	reqURL := fmt.Sprintf("%s/api/v0/transfers/downloads/%s", d.baseURL, url.PathEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slskd enqueue returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (d *Downloader) waitForDownload(ctx context.Context, username string, enqueuedFiles []FilePayload, progress downloader.ProgressFunc) error {
	fileMap := make(map[string]bool)
	var totalBytes int64
	for _, f := range enqueuedFiles {
		fileMap[f.Filename] = true
		totalBytes += f.Size
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastProgressTime := time.Now()
	var lastBytesTransferred int64

	for {
		select {
		case <-ctx.Done():
			d.cancelUserDownloads(context.Background(), username, enqueuedFiles)
			return ctx.Err()
		case <-ticker.C:
			userTransfers, err := d.getUserTransfers(ctx, username)
			if err != nil {
				continue
			}

			var currentTransferred int64
			completedCount := 0
			erroredCount := 0
			var errMsgs []string

			for _, dir := range userTransfers.Directories {
				for _, tr := range dir.Files {
					if !fileMap[tr.Filename] {
						continue
					}

					currentTransferred += tr.BytesTransferred

					switch tr.State {
					case "Completed, Succeeded":
						completedCount++
					case "Completed, Cancelled", "Completed, TimedOut", "Completed, Errored", "Completed, Rejected", "Completed, Aborted":
						erroredCount++
						errMsgs = append(errMsgs, fmt.Sprintf("%s (%s)", filepath.Base(tr.Filename), tr.State))
					}
				}
			}

			if currentTransferred > lastBytesTransferred {
				lastProgressTime = time.Now()
				lastBytesTransferred = currentTransferred
			}

			if progress != nil && totalBytes > 0 {
				ratio := float64(currentTransferred) / float64(totalBytes)
				if ratio > 1.0 {
					ratio = 1.0
				}
				pct := 10.0 + 80.0*ratio
				progress(pct, fmt.Sprintf("Downloading [%d/%d tracks completed] (%.1f MB / %.1f MB)...",
					completedCount, len(enqueuedFiles), float64(currentTransferred)/1048576, float64(totalBytes)/1048576))
			}

			if completedCount == len(enqueuedFiles) {
				return nil
			}

			if erroredCount > 0 && erroredCount+completedCount == len(enqueuedFiles) {
				return fmt.Errorf("soulseek transfers failed: %s", strings.Join(errMsgs, ", "))
			}

			if time.Since(lastProgressTime) > 10*time.Minute {
				return fmt.Errorf("soulseek download stalled for more than 10 minutes from peer %s", username)
			}
		}
	}
}

func (d *Downloader) getUserTransfers(ctx context.Context, username string) (*SlskdUserTransfers, error) {
	reqURL := fmt.Sprintf("%s/api/v0/transfers/downloads/%s", d.baseURL, url.PathEscape(username))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get user transfers status %d", resp.StatusCode)
	}

	var res SlskdUserTransfers
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (d *Downloader) collectDownloadedFiles(username, folderName string, files []FilePayload, targetDir string) ([]string, error) {
	var collected []string
	searchDirs := []string{
		filepath.Join(d.downloadDir, folderName),
		filepath.Join(d.downloadDir, username, folderName),
		filepath.Join(d.downloadDir, username),
		d.downloadDir,
	}

	for _, f := range files {
		norm := strings.ReplaceAll(f.Filename, "\\", "/")
		baseName := filepath.Base(norm)

		var foundPath string
		for _, dir := range searchDirs {
			candidate := filepath.Join(dir, baseName)
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				foundPath = candidate
				break
			}
		}

		if foundPath == "" {
			_ = filepath.Walk(d.downloadDir, func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() && filepath.Base(path) == baseName {
					foundPath = path
					return filepath.SkipAll
				}
				return nil
			})
		}

		if foundPath == "" {
			return nil, fmt.Errorf("downloaded file '%s' not found on disk in %s", baseName, d.downloadDir)
		}

		destPath := filepath.Join(targetDir, baseName)
		if err := os.Rename(foundPath, destPath); err != nil {
			if err := copyFile(foundPath, destPath); err != nil {
				return nil, fmt.Errorf("move file from %s to %s: %w", foundPath, destPath, err)
			}
			_ = os.Remove(foundPath)
		}
		collected = append(collected, destPath)
	}

	// Clean up empty directories
	for _, dir := range searchDirs {
		if dir != d.downloadDir && dir != "" {
			_ = os.Remove(dir)
		}
	}

	return collected, nil
}

func (d *Downloader) cancelUserDownloads(ctx context.Context, username string, files []FilePayload) {
	transfers, err := d.getUserTransfers(ctx, username)
	if err != nil {
		return
	}
	fileMap := make(map[string]bool)
	for _, f := range files {
		fileMap[f.Filename] = true
	}
	for _, dir := range transfers.Directories {
		for _, tr := range dir.Files {
			if fileMap[tr.Filename] && tr.ID != "" {
				delURL := fmt.Sprintf("%s/api/v0/transfers/downloads/%s/%s", d.baseURL, url.PathEscape(username), tr.ID)
				req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
				if d.apiKey != "" {
					req.Header.Set("X-API-Key", d.apiKey)
				}
				resp, err := d.httpClient.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func isAudioExtension(ext string) bool {
	switch ext {
	case ".flac", ".mp3", ".m4a", ".wav", ".aif", ".aiff", ".ogg":
		return true
	default:
		return false
	}
}
