package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
	"github.com/findr-app/findr/internal/importer"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/navidrome"
	"github.com/findr-app/findr/internal/storage"
	"github.com/findr-app/findr/internal/verifier"
)

// Orchestrator coordinates metadata indexing, downloading, verification, importing, and server notifications.
type Orchestrator struct {
	mu         sync.RWMutex
	cfg        *config.Config
	repo       storage.Repository
	indexer    indexer.Indexer
	downloader downloader.Downloader
	verifier   *verifier.Verifier
	importer   *importer.Importer
	navidrome  *navidrome.Client
	queueMgr   *QueueManager
}

// NewOrchestrator creates a new pipeline orchestrator.
func NewOrchestrator(
	cfg *config.Config,
	repo storage.Repository,
	idx indexer.Indexer,
	dl downloader.Downloader,
	vr *verifier.Verifier,
	imp *importer.Importer,
	navi *navidrome.Client,
) *Orchestrator {
	maxConcurrent := 2
	if cfg != nil && cfg.Downloader.MaxConcurrentDownloads > 0 {
		maxConcurrent = cfg.Downloader.MaxConcurrentDownloads
	}
	orch := &Orchestrator{
		cfg:        cfg,
		repo:       repo,
		indexer:    idx,
		downloader: dl,
		verifier:   vr,
		importer:   imp,
		navidrome:  navi,
	}
	orch.queueMgr = NewQueueManager(orch, maxConcurrent)
	orch.queueMgr.Start()
	return orch
}

// ArtistDownloadSummary summarizes batch download and import of an artist's discography.
type ArtistDownloadSummary struct {
	ArtistID      string            `json:"artist_id"`
	ArtistName    string            `json:"artist_name"`
	TotalReleases int               `json:"total_releases"`
	Successful    int               `json:"successful"`
	Failed        int               `json:"failed"`
	Skipped       int               `json:"skipped"` // Already completed
	ImportedPaths []string          `json:"imported_paths"`
	Errors        map[string]string `json:"errors,omitempty"`
}

// SearchArtists searches artists on the indexer and identifies if they are already in the database.
func (o *Orchestrator) SearchArtists(ctx context.Context, query string) ([]domain.Artist, error) {
	o.mu.RLock()
	idx := o.indexer
	o.mu.RUnlock()

	artists, err := idx.SearchArtist(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search artists '%s': %w", query, err)
	}

	monitored, err := o.repo.ListArtists(ctx)
	if err == nil {
		monitoredMap := make(map[string]bool)
		for _, m := range monitored {
			monitoredMap[m.ID] = true
			if m.SourceID != "" {
				monitoredMap[m.SourceID] = true
			}
		}
		for i := range artists {
			if monitoredMap[artists[i].ID] || monitoredMap[artists[i].SourceID] {
				artists[i].Monitored = true
			}
		}
	}

	return artists, nil
}

// GetArtistReleasesFromIndexer fetches discography directly from the indexer for previewing.
func (o *Orchestrator) GetArtistReleasesFromIndexer(ctx context.Context, artistID string) ([]domain.Album, error) {
	o.mu.RLock()
	idx := o.indexer
	o.mu.RUnlock()

	return idx.GetArtistReleases(ctx, artistID, indexer.ReleaseFilter{Limit: 100})
}

// AddArtist adds a selected artist, indexes their releases, and optionally triggers batch download.
func (o *Orchestrator) AddArtist(ctx context.Context, artist *domain.Artist, monitor bool, downloadAll bool) (*domain.Artist, []domain.Album, error) {
	o.mu.RLock()
	idx := o.indexer
	o.mu.RUnlock()

	if (artist.Name == "" || artist.ImageURL == "") && artist.SourceID != "" {
		fetched, err := idx.GetArtist(ctx, artist.SourceID)
		if err == nil && fetched != nil {
			if artist.Name == "" {
				artist.Name = fetched.Name
			}
			if artist.ImageURL == "" {
				artist.ImageURL = fetched.ImageURL
			}
			if artist.Source == "" {
				artist.Source = fetched.Source
			}
		}
	}
	if artist.ID == "" {
		artist.ID = artist.SourceID
	}
	artist.Monitored = monitor

	if err := o.repo.SaveArtist(ctx, artist); err != nil {
		return nil, nil, fmt.Errorf("save artist: %w", err)
	}

	// Fetch discography (Albums, EPs, Singles)
	releases, err := idx.GetArtistReleases(ctx, artist.SourceID, indexer.ReleaseFilter{Limit: 100})
	if err != nil {
		return artist, nil, fmt.Errorf("fetch artist releases: %w", err)
	}

	for i := range releases {
		rel := &releases[i]
		rel.ArtistID = artist.ID
		rel.ArtistName = artist.Name
		rel.Monitored = monitor
		if err := o.repo.SaveAlbum(ctx, rel); err != nil {
			return artist, releases, fmt.Errorf("save album %s: %w", rel.Title, err)
		}
	}

	if downloadAll {
		go func() {
			bgCtx := context.Background()
			_, _ = o.DownloadArtistReleases(bgCtx, artist.ID, nil, nil)
		}()
	}

	return artist, releases, nil
}

// TrackAndDownloadArtist searches an artist, adds them to database, and downloads their entire discography.
func (o *Orchestrator) TrackAndDownloadArtist(ctx context.Context, query string, releaseTypes []domain.ReleaseType, progress downloader.ProgressFunc) (*domain.Artist, *ArtistDownloadSummary, error) {
	artist, _, err := o.SearchAndAddArtist(ctx, query)
	if err != nil {
		return nil, nil, err
	}

	summary, err := o.DownloadArtistReleases(ctx, artist.ID, releaseTypes, progress)
	return artist, summary, err
}

// DownloadArtistReleases downloads and imports all monitored releases (Albums, EPs, Singles) for an artist.
func (o *Orchestrator) DownloadArtistReleases(ctx context.Context, artistID string, releaseTypes []domain.ReleaseType, progress downloader.ProgressFunc) (*ArtistDownloadSummary, error) {
	artist, err := o.repo.GetArtist(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("retrieve artist %s: %w", artistID, err)
	}
	if artist == nil {
		return nil, fmt.Errorf("artist %s not found", artistID)
	}

	albums, err := o.repo.ListAlbumsByArtist(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("list albums for artist %s: %w", artistID, err)
	}

	typeFilterMap := make(map[domain.ReleaseType]bool)
	for _, t := range releaseTypes {
		typeFilterMap[t] = true
	}

	summary := &ArtistDownloadSummary{
		ArtistID:      artist.ID,
		ArtistName:    artist.Name,
		TotalReleases: len(albums),
		ImportedPaths: make([]string, 0),
		Errors:        make(map[string]string),
	}

	importedCount := 0
	for i, alb := range albums {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		if len(typeFilterMap) > 0 && !typeFilterMap[alb.ReleaseType] {
			continue
		}

		if alb.Status == domain.StatusCompleted {
			summary.Skipped++
			continue
		}

		if progress != nil {
			progress(float64(i)/float64(len(albums))*100.0,
				fmt.Sprintf("Processing [%d/%d] (%s): %s", i+1, len(albums), alb.ReleaseType, alb.Title))
		}

		_, impResult, err := o.ProcessAlbumPipeline(ctx, alb.ID, progress)
		if err != nil {
			summary.Failed++
			summary.Errors[alb.ID] = fmt.Sprintf("%s: %v", alb.Title, err)
			continue
		}

		summary.Successful++
		importedCount++
		if impResult != nil {
			summary.ImportedPaths = append(summary.ImportedPaths, impResult.AlbumDir)
		}
	}

	// Trigger Navidrome scan once after the entire batch finishes
	o.mu.RLock()
	navi := o.navidrome
	naviEnabled := o.cfg.Navidrome.Enabled
	o.mu.RUnlock()

	if importedCount > 0 && naviEnabled && navi != nil {
		if progress != nil {
			progress(99.0, "Triggering Navidrome library scan for imported releases...")
		}
		_, _ = navi.StartScan(ctx, false)
	}

	if progress != nil {
		progress(100.0, fmt.Sprintf("Completed artist discography download: %d imported, %d skipped, %d failed",
			summary.Successful, summary.Skipped, summary.Failed))
	}

	return summary, nil
}

// SearchAndAddArtist finds the first matching artist and adds them.
func (o *Orchestrator) SearchAndAddArtist(ctx context.Context, query string) (*domain.Artist, []domain.Album, error) {
	artists, err := o.SearchArtists(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	if len(artists) == 0 {
		return nil, nil, fmt.Errorf("no artist found for '%s'", query)
	}

	selected := artists[0]
	return o.AddArtist(ctx, &selected, true, false)
}

// ProcessAlbumPipeline downloads, verifies, imports, and triggers Navidrome rescan for an album.
func (o *Orchestrator) ProcessAlbumPipeline(ctx context.Context, albumID string, progress downloader.ProgressFunc) (*domain.MatchResult, *importer.ImportResult, error) {
	return o.ProcessAlbumPipelineWithJob(ctx, albumID, "", progress)
}

// ProcessAlbumPipelineWithJob downloads, verifies, imports, and updates an existing download job.
func (o *Orchestrator) ProcessAlbumPipelineWithJob(ctx context.Context, albumID string, existingJobID string, progress downloader.ProgressFunc) (*domain.MatchResult, *importer.ImportResult, error) {
	album, err := o.repo.GetAlbum(ctx, albumID)
	if err != nil {
		return nil, nil, fmt.Errorf("retrieve album %s: %w", albumID, err)
	}
	if album == nil {
		return nil, nil, fmt.Errorf("album %s not found in repository", albumID)
	}

	o.mu.RLock()
	idx := o.indexer
	dl := o.downloader
	vr := o.verifier
	imp := o.importer
	navi := o.navidrome
	cfg := o.cfg
	o.mu.RUnlock()

	// 1. Ensure full tracklist and UPC is present
	if len(album.Tracks) == 0 || album.UPC == "" {
		if progress != nil {
			progress(5.0, "Fetching full tracklist from indexer...")
		}
		detailedAlbum, err := idx.GetAlbumDetails(ctx, album.SourceID)
		if err == nil && detailedAlbum != nil {
			album.Tracks = detailedAlbum.Tracks
			album.TotalTracks = detailedAlbum.TotalTracks
			if detailedAlbum.CoverURL != "" {
				album.CoverURL = detailedAlbum.CoverURL
			}
			if detailedAlbum.UPC != "" {
				album.UPC = detailedAlbum.UPC
			}
			_ = o.repo.SaveAlbum(ctx, album)
		}
	}

	for i := range album.Tracks {
		if album.Tracks[i].TrackNumber <= 0 {
			album.Tracks[i].TrackNumber = i + 1
		}
		if album.Tracks[i].DiscNumber <= 0 {
			album.Tracks[i].DiscNumber = 1
		}
	}

	// 2. Download release into staging folder
	stagingDir := filepath.Join(cfg.Library.DownloadPath, fmt.Sprintf("%s-%d", album.ID, time.Now().Unix()))
	album.Status = domain.StatusDownloading
	_ = o.repo.SaveAlbum(ctx, album)
	fmt.Printf("[findr Pipeline] Starting download for '%s - %s' into %s\n", album.ArtistName, album.Title, stagingDir)

	jobID := existingJobID
	var downloadJob *domain.DownloadJob
	if jobID != "" {
		downloadJob, _ = o.repo.GetDownloadJob(ctx, jobID)
	}
	if downloadJob == nil {
		if jobID == "" {
			jobID = fmt.Sprintf("dl-%s-%d", album.ID, time.Now().Unix())
		}
		downloadJob = &domain.DownloadJob{
			ID:            jobID,
			AlbumID:       album.ID,
			Downloader:    cfg.Downloader.Preferred,
			DownloadDir:   stagingDir,
			TargetQuality: domain.AudioQuality(cfg.Downloader.Qobuz.TargetQuality),
			Status:        domain.StatusDownloading,
			Progress:      0.0,
			CreatedAt:     time.Now(),
		}
	} else {
		downloadJob.Status = domain.StatusDownloading
		downloadJob.DownloadDir = stagingDir
		if downloadJob.Downloader == "" {
			downloadJob.Downloader = cfg.Downloader.Preferred
		}
	}
	_ = o.repo.SaveDownloadJob(ctx, downloadJob)

	trackedProgress := func(pct float64, msg string) {
		downloadJob.Progress = pct
		_ = o.repo.SaveDownloadJob(ctx, downloadJob)
		if progress != nil {
			progress(pct, msg)
		}
	}

	dlResult, err := dl.Download(ctx, album, stagingDir, trackedProgress)
	if err != nil {
		album.Status = domain.StatusFailed
		downloadJob.Status = domain.StatusFailed
		downloadJob.ErrorMessage = err.Error()
		_ = o.repo.SaveAlbum(ctx, album)
		_ = o.repo.SaveDownloadJob(ctx, downloadJob)
		fmt.Printf("[findr Error] Download failed for '%s - %s': %v\n", album.ArtistName, album.Title, err)
		return nil, nil, fmt.Errorf("download failed: %w", err)
	}
	if dlResult != nil {
		downloadJob.DownloadedFiles = dlResult.DownloadedFiles
	}
	_ = o.repo.SaveDownloadJob(ctx, downloadJob)

	// 3. Scan and Verify files against Indexer metadata
	album.Status = domain.StatusVerifying
	downloadJob.Status = domain.StatusVerifying
	_ = o.repo.SaveAlbum(ctx, album)
	_ = o.repo.SaveDownloadJob(ctx, downloadJob)
	if progress != nil {
		progress(75.0, "Verifying downloaded audio files against indexer metadata...")
	}

	candidates, err := vr.ScanCandidateFiles(stagingDir)
	if err != nil {
		album.Status = domain.StatusFailed
		downloadJob.Status = domain.StatusFailed
		downloadJob.ErrorMessage = fmt.Sprintf("scan candidate files failed: %v", err)
		_ = o.repo.SaveAlbum(ctx, album)
		_ = o.repo.SaveDownloadJob(ctx, downloadJob)
		fmt.Printf("[findr Error] File scan failed for '%s - %s': %v\n", album.ArtistName, album.Title, err)
		return nil, nil, fmt.Errorf("scan candidates: %w", err)
	}

	matchResult, err := vr.VerifyAlbum(ctx, album, candidates)
	if err != nil {
		album.Status = domain.StatusFailed
		downloadJob.Status = domain.StatusFailed
		downloadJob.ErrorMessage = fmt.Sprintf("verification error: %v", err)
		_ = o.repo.SaveAlbum(ctx, album)
		_ = o.repo.SaveDownloadJob(ctx, downloadJob)
		fmt.Printf("[findr Error] Verification error for '%s - %s': %v\n", album.ArtistName, album.Title, err)
		return nil, nil, fmt.Errorf("verify album: %w", err)
	}

	if !matchResult.Passed {
		album.Status = domain.StatusFailed
		downloadJob.Status = domain.StatusFailed
		downloadJob.ErrorMessage = fmt.Sprintf("verification rejected: %s", matchResult.Reason)
		_ = o.repo.SaveAlbum(ctx, album)
		_ = o.repo.SaveDownloadJob(ctx, downloadJob)
		fmt.Printf("[findr Error] Verification rejected for '%s - %s': %s\n", album.ArtistName, album.Title, matchResult.Reason)
		return matchResult, nil, fmt.Errorf("verification rejected: %s", matchResult.Reason)
	}

	// 4. Import files into Library
	album.Status = domain.StatusImporting
	downloadJob.Status = domain.StatusImporting
	_ = o.repo.SaveAlbum(ctx, album)
	_ = o.repo.SaveDownloadJob(ctx, downloadJob)
	if progress != nil {
		progress(90.0, "Importing verified audio files into library...")
	}

	importResult, err := imp.ImportAlbum(ctx, album, matchResult)
	if err != nil {
		album.Status = domain.StatusFailed
		downloadJob.Status = domain.StatusFailed
		downloadJob.ErrorMessage = fmt.Sprintf("import failed: %v", err)
		_ = o.repo.SaveAlbum(ctx, album)
		_ = o.repo.SaveDownloadJob(ctx, downloadJob)
		fmt.Printf("[findr Error] Import failed for '%s - %s': %v\n", album.ArtistName, album.Title, err)
		return matchResult, nil, fmt.Errorf("import failed: %w", err)
	}

	// Ensure artist artwork (artist.jpg) is present in the artist folder for Navidrome/media servers
	if imp != nil && importResult != nil && importResult.AlbumDir != "" {
		artistDir := filepath.Dir(importResult.AlbumDir)
		artistJpg := filepath.Join(artistDir, "artist.jpg")
		if _, statErr := os.Stat(artistJpg); os.IsNotExist(statErr) {
			var artistImgURL string
			if album.ArtistID != "" {
				if art, err := o.repo.GetArtist(ctx, album.ArtistID); err == nil && art != nil {
					artistImgURL = art.ImageURL
				}
			}
			if artistImgURL == "" && album.ArtistName != "" {
				if allArtists, err := o.repo.ListArtists(ctx); err == nil {
					for _, a := range allArtists {
						if strings.EqualFold(a.Name, album.ArtistName) && a.ImageURL != "" {
							artistImgURL = a.ImageURL
							break
						}
					}
				}
			}
			if artistImgURL != "" {
				if err := imp.DownloadImage(ctx, artistImgURL, artistJpg); err != nil {
					fmt.Printf("[importer Warning] Failed to download artist image from %s: %v\n", artistImgURL, err)
				}
			}
		}
	}

	// Clean up temporary download staging folder if configured
	if cfg.Import.DeleteAfterImport && stagingDir != "" {
		_ = os.RemoveAll(stagingDir)
	}

	// 5. Trigger Navidrome rescan if enabled
	if cfg.Navidrome.Enabled && navi != nil {
		if progress != nil {
			progress(98.0, "Triggering Navidrome library scan...")
		}
		_, err := navi.StartScan(ctx, false)
		if err != nil {
			fmt.Printf("[Warning] Failed to trigger Navidrome scan: %v\n", err)
		}
	}

	now := time.Now()
	album.Status = domain.StatusCompleted
	downloadJob.Status = domain.StatusCompleted
	downloadJob.CompletedAt = &now
	downloadJob.Progress = 100.0
	_ = o.repo.SaveAlbum(ctx, album)
	_ = o.repo.SaveDownloadJob(ctx, downloadJob)

	fmt.Printf("[findr Success] Successfully verified and imported '%s - %s' into %s\n",
		album.ArtistName, album.Title, importResult.AlbumDir)

	if progress != nil {
		progress(100.0, fmt.Sprintf("Album successfully imported into %s", importResult.AlbumDir))
	}

	return matchResult, importResult, nil
}

// GetDownloadQueue returns all currently active download jobs.
func (o *Orchestrator) GetDownloadQueue(ctx context.Context) ([]domain.DownloadJob, error) {
	return o.repo.ListActiveDownloadJobs(ctx)
}

// GetDownloadHistory returns historical download jobs.
func (o *Orchestrator) GetDownloadHistory(ctx context.Context, limit int) ([]domain.DownloadJob, error) {
	return o.repo.ListDownloadJobs(ctx, limit)
}

// ClearDownloadHistory removes completed and failed download jobs from history.
func (o *Orchestrator) ClearDownloadHistory(ctx context.Context) error {
	return o.repo.ClearDownloadHistory(ctx)
}

// DeleteArtist removes an artist and their albums from the local repository.
func (o *Orchestrator) DeleteArtist(ctx context.Context, artistID string) error {
	return o.repo.DeleteArtist(ctx, artistID)
}

// UpdateConfig hot-reloads runtime configuration and service references.
func (o *Orchestrator) UpdateConfig(
	cfg *config.Config,
	idx indexer.Indexer,
	dl downloader.Downloader,
	vr *verifier.Verifier,
	imp *importer.Importer,
	navi *navidrome.Client,
) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.cfg = cfg
	if idx != nil {
		o.indexer = idx
	}
	if dl != nil {
		o.downloader = dl
	}
	if vr != nil {
		o.verifier = vr
	}
	if imp != nil {
		o.importer = imp
	}
	o.navidrome = navi // can be nil if disabled

	if o.queueMgr != nil && cfg != nil && cfg.Downloader.MaxConcurrentDownloads > 0 {
		o.queueMgr.SetMaxConcurrent(cfg.Downloader.MaxConcurrentDownloads)
	}
}

// EnqueueAlbumDownload adds a single album to the download queue.
func (o *Orchestrator) EnqueueAlbumDownload(albumID string) (string, error) {
	if o.queueMgr == nil {
		return "", fmt.Errorf("queue manager not initialized")
	}
	return o.queueMgr.Enqueue(albumID)
}

// EnqueueBatchDownloads adds multiple albums to the download queue.
func (o *Orchestrator) EnqueueBatchDownloads(albumIDs []string) (int, error) {
	if o.queueMgr == nil {
		return 0, fmt.Errorf("queue manager not initialized")
	}
	return o.queueMgr.EnqueueBatch(albumIDs)
}

// EnqueueArtistReleases finds all uncompleted releases for an artist and enqueues them for concurrent downloading.
func (o *Orchestrator) EnqueueArtistReleases(ctx context.Context, artistID string, releaseTypes []domain.ReleaseType) (int, error) {
	albums, err := o.repo.ListAlbumsByArtist(ctx, artistID)
	if err != nil {
		return 0, fmt.Errorf("list albums for artist %s: %w", artistID, err)
	}

	typeFilterMap := make(map[domain.ReleaseType]bool)
	for _, t := range releaseTypes {
		typeFilterMap[t] = true
	}

	var toEnqueue []string
	for _, alb := range albums {
		if len(typeFilterMap) > 0 && !typeFilterMap[alb.ReleaseType] {
			continue
		}
		if alb.Status == domain.StatusCompleted {
			continue
		}
		toEnqueue = append(toEnqueue, alb.ID)
	}

	if o.queueMgr == nil {
		return 0, fmt.Errorf("queue manager not initialized")
	}

	return o.queueMgr.EnqueueBatch(toEnqueue)
}

// CancelDownload cancels an ongoing or pending download job.
func (o *Orchestrator) CancelDownload(albumID string) error {
	if o.queueMgr == nil {
		return fmt.Errorf("queue manager not initialized")
	}
	return o.queueMgr.Cancel(albumID)
}

// GetQueueStatus returns the current concurrency limit, active downloads count, and pending queue count.
func (o *Orchestrator) GetQueueStatus() (max int, active int, pending int) {
	if o.queueMgr == nil {
		return 0, 0, 0
	}
	return o.queueMgr.GetConcurrency()
}

// EnsureAlbumTracks checks if the album has tracks loaded; if not, fetches from indexer and persists.
func (o *Orchestrator) EnsureAlbumTracks(ctx context.Context, albumID string) (*domain.Album, error) {
	album, err := o.repo.GetAlbum(ctx, albumID)
	if err != nil {
		return nil, fmt.Errorf("retrieve album %s: %w", albumID, err)
	}
	if album == nil {
		return nil, fmt.Errorf("album %s not found", albumID)
	}

	if len(album.Tracks) > 0 {
		return album, nil
	}

	if album.SourceID == "" {
		return album, nil
	}

	o.mu.RLock()
	idx := o.indexer
	o.mu.RUnlock()

	if idx == nil {
		return album, nil
	}

	detailed, err := idx.GetAlbumDetails(ctx, album.SourceID)
	if err == nil && detailed != nil {
		album.Tracks = detailed.Tracks
		album.TotalTracks = detailed.TotalTracks
		if detailed.CoverURL != "" && album.CoverURL == "" {
			album.CoverURL = detailed.CoverURL
		}
		if detailed.UPC != "" && album.UPC == "" {
			album.UPC = detailed.UPC
		}
		for i := range album.Tracks {
			if album.Tracks[i].TrackNumber <= 0 {
				album.Tracks[i].TrackNumber = i + 1
			}
			if album.Tracks[i].DiscNumber <= 0 {
				album.Tracks[i].DiscNumber = 1
			}
			if album.Tracks[i].ArtistName == "" {
				album.Tracks[i].ArtistName = album.ArtistName
			}
		}
		_ = o.repo.SaveAlbum(ctx, album)
	}

	return album, nil
}

// GetArtistWithReleases returns an artist and their categorized discography.
func (o *Orchestrator) GetArtistWithReleases(ctx context.Context, artistID string) (*domain.Artist, []domain.Album, error) {
	artist, err := o.repo.GetArtist(ctx, artistID)
	if err != nil {
		return nil, nil, fmt.Errorf("retrieve artist %s: %w", artistID, err)
	}
	if artist == nil {
		return nil, nil, fmt.Errorf("artist %s not found", artistID)
	}

	albums, err := o.repo.ListAlbumsByArtist(ctx, artistID)
	if err != nil {
		return artist, nil, fmt.Errorf("list albums for artist %s: %w", artistID, err)
	}

	return artist, albums, nil
}

// StopQueue shuts down the internal queue manager.
func (o *Orchestrator) StopQueue() {
	if o.queueMgr != nil {
		o.queueMgr.Stop()
	}
}
