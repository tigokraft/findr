package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
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
	cfg        *config.Config
	repo       storage.Repository
	indexer    indexer.Indexer
	downloader downloader.Downloader
	verifier   *verifier.Verifier
	importer   *importer.Importer
	navidrome  *navidrome.Client
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
	return &Orchestrator{
		cfg:        cfg,
		repo:       repo,
		indexer:    idx,
		downloader: dl,
		verifier:   vr,
		importer:   imp,
		navidrome:  navi,
	}
}

// SearchAndAddArtist finds an artist via indexer, saves them to database, and pulls their discography.
func (o *Orchestrator) SearchAndAddArtist(ctx context.Context, query string) (*domain.Artist, []domain.Album, error) {
	artists, err := o.indexer.SearchArtist(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("search artist '%s': %w", query, err)
	}
	if len(artists) == 0 {
		return nil, nil, fmt.Errorf("no artist found for '%s'", query)
	}

	selected := artists[0]
	selected.Monitored = true

	if err := o.repo.SaveArtist(ctx, &selected); err != nil {
		return nil, nil, fmt.Errorf("save artist: %w", err)
	}

	// Fetch discography (Albums, EPs, Singles)
	releases, err := o.indexer.GetArtistReleases(ctx, selected.ID, indexer.ReleaseFilter{Limit: 100})
	if err != nil {
		return &selected, nil, fmt.Errorf("fetch artist releases: %w", err)
	}

	for i := range releases {
		rel := &releases[i]
		rel.ArtistID = selected.ID
		rel.ArtistName = selected.Name
		rel.Monitored = true
		if err := o.repo.SaveAlbum(ctx, rel); err != nil {
			return &selected, releases, fmt.Errorf("save album %s: %w", rel.Title, err)
		}
	}

	return &selected, releases, nil
}

// ProcessAlbumPipeline downloads, verifies, imports, and triggers Navidrome rescan for an album.
func (o *Orchestrator) ProcessAlbumPipeline(ctx context.Context, albumID string, progress downloader.ProgressFunc) (*domain.MatchResult, *importer.ImportResult, error) {
	album, err := o.repo.GetAlbum(ctx, albumID)
	if err != nil {
		return nil, nil, fmt.Errorf("retrieve album %s: %w", albumID, err)
	}
	if album == nil {
		return nil, nil, fmt.Errorf("album %s not found in repository", albumID)
	}

	// 1. Ensure full tracklist is present
	if len(album.Tracks) == 0 {
		if progress != nil {
			progress(5.0, "Fetching full tracklist from indexer...")
		}
		detailedAlbum, err := o.indexer.GetAlbumDetails(ctx, album.SourceID)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch album details: %w", err)
		}
		album.Tracks = detailedAlbum.Tracks
		album.TotalTracks = detailedAlbum.TotalTracks
		album.CoverURL = detailedAlbum.CoverURL
		album.UPC = detailedAlbum.UPC
		_ = o.repo.SaveAlbum(ctx, album)
	}

	// 2. Download release into staging folder
	stagingDir := filepath.Join(o.cfg.Library.DownloadPath, fmt.Sprintf("%s-%d", album.ID, time.Now().Unix()))
	album.Status = domain.StatusDownloading
	_ = o.repo.SaveAlbum(ctx, album)

	downloadJob, err := o.downloader.Download(ctx, album, stagingDir, progress)
	if err != nil {
		album.Status = domain.StatusFailed
		_ = o.repo.SaveAlbum(ctx, album)
		return nil, nil, fmt.Errorf("download failed: %w", err)
	}
	_ = o.repo.SaveDownloadJob(ctx, downloadJob)

	// 3. Scan and Verify files against Indexer metadata
	album.Status = domain.StatusVerifying
	_ = o.repo.SaveAlbum(ctx, album)
	if progress != nil {
		progress(75.0, "Verifying downloaded audio files against indexer metadata...")
	}

	candidates, err := o.verifier.ScanCandidateFiles(stagingDir)
	if err != nil {
		return nil, nil, fmt.Errorf("scan candidates: %w", err)
	}

	matchResult, err := o.verifier.VerifyAlbum(ctx, album, candidates)
	if err != nil {
		return nil, nil, fmt.Errorf("verify album: %w", err)
	}

	if !matchResult.Passed {
		album.Status = domain.StatusFailed
		_ = o.repo.SaveAlbum(ctx, album)
		return matchResult, nil, fmt.Errorf("verification rejected: %s", matchResult.Reason)
	}

	// 4. Import files into Library
	album.Status = domain.StatusImporting
	_ = o.repo.SaveAlbum(ctx, album)
	if progress != nil {
		progress(90.0, "Importing verified audio files into library...")
	}

	importResult, err := o.importer.ImportAlbum(ctx, album, matchResult)
	if err != nil {
		album.Status = domain.StatusFailed
		_ = o.repo.SaveAlbum(ctx, album)
		return matchResult, nil, fmt.Errorf("import failed: %w", err)
	}

	// 5. Trigger Navidrome rescan if enabled
	if o.cfg.Navidrome.Enabled && o.navidrome != nil {
		if progress != nil {
			progress(98.0, "Triggering Navidrome library scan...")
		}
		_, err := o.navidrome.StartScan(ctx, false)
		if err != nil {
			fmt.Printf("[Warning] Failed to trigger Navidrome scan: %v\n", err)
		}
	}

	album.Status = domain.StatusCompleted
	_ = o.repo.SaveAlbum(ctx, album)

	if progress != nil {
		progress(100.0, fmt.Sprintf("Album successfully imported into %s", importResult.AlbumDir))
	}

	return matchResult, importResult, nil
}
