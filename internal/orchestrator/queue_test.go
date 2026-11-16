package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/storage"
)

type mockIndexer struct{}

func (m *mockIndexer) Name() string { return "mock-indexer" }
func (m *mockIndexer) SearchArtist(ctx context.Context, query string) ([]domain.Artist, error) {
	return nil, nil
}
func (m *mockIndexer) GetArtist(ctx context.Context, id string) (*domain.Artist, error) {
	return nil, nil
}
func (m *mockIndexer) GetArtistReleases(ctx context.Context, artistID string, filter indexer.ReleaseFilter) ([]domain.Album, error) {
	return nil, nil
}
func (m *mockIndexer) GetAlbumDetails(ctx context.Context, albumID string) (*domain.Album, error) {
	return &domain.Album{
		ID:       albumID,
		ArtistID: "art-1",
		SourceID: albumID,
		Title:    "Test Album " + albumID,
		Tracks: []domain.Track{
			{ID: "t1", Title: "Track 1", DurationSec: 180, TrackNumber: 1},
		},
		TotalTracks: 1,
	}, nil
}

type mockDownloader struct {
	activeCount atomic.Int64
	maxObserved atomic.Int64
	blockCh     chan struct{}
	delay       time.Duration
}

func newMockDownloader(delay time.Duration, blockCh chan struct{}) *mockDownloader {
	return &mockDownloader{
		delay:   delay,
		blockCh: blockCh,
	}
}

func (m *mockDownloader) Name() string { return "mock-dl" }
func (m *mockDownloader) Search(ctx context.Context, album *domain.Album) ([]downloader.SearchResult, error) {
	return []downloader.SearchResult{
		{ID: "search-1", Title: album.Title, Source: "mock-dl"},
	}, nil
}
func (m *mockDownloader) Download(ctx context.Context, album *domain.Album, targetDir string, progress downloader.ProgressFunc) (*domain.DownloadJob, error) {
	_ = os.MkdirAll(targetDir, 0755)
	cur := m.activeCount.Add(1)
	defer m.activeCount.Add(-1)

	for {
		old := m.maxObserved.Load()
		if cur <= old || m.maxObserved.CompareAndSwap(old, cur) {
			break
		}
	}

	if m.blockCh != nil {
		select {
		case <-m.blockCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return &domain.DownloadJob{
		ID:       fmt.Sprintf("job-%s", album.ID),
		AlbumID:  album.ID,
		Status:   domain.StatusCompleted,
		Progress: 100.0,
	}, nil
}

func setupTestOrchestrator(t *testing.T, maxConcurrent int, dl *mockDownloader) (*Orchestrator, storage.Repository) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	repo, err := storage.NewSQLiteRepository(dbPath)
	if err != nil {
		t.Fatalf("failed to init test repo: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Library.MusicPath = filepath.Join(tmpDir, "music")
	cfg.Library.DownloadPath = filepath.Join(tmpDir, "downloads")
	cfg.Downloader.MaxConcurrentDownloads = maxConcurrent

	orch := NewOrchestrator(cfg, repo, &mockIndexer{}, dl, nil, nil, nil)
	return orch, repo
}

func TestQueueManager_ConcurrencyLimit(t *testing.T) {
	dl := newMockDownloader(40*time.Millisecond, nil)
	orch, repo := setupTestOrchestrator(t, 2, dl)
	defer orch.queueMgr.Stop()

	ctx := context.Background()

	// Seed artist
	art := &domain.Artist{
		ID:        "art-1",
		Name:      "Test Artist",
		Source:    "deezer",
		SourceID:  "src-1",
		Monitored: true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.SaveArtist(ctx, art); err != nil {
		t.Fatalf("failed to save artist: %v", err)
	}

	// Seed 4 albums
	for i := 1; i <= 4; i++ {
		alb := &domain.Album{
			ID:          fmt.Sprintf("alb-%d", i),
			ArtistID:    "art-1",
			SourceID:    fmt.Sprintf("src-%d", i),
			Title:       fmt.Sprintf("Album %d", i),
			ArtistName:  "Test Artist",
			ReleaseType: domain.ReleaseTypeAlbum,
			Status:      domain.DownloadStatus("wanted"),
			Tracks: []domain.Track{
				{ID: fmt.Sprintf("t-%d", i), Title: "T1", DurationSec: 100, TrackNumber: 1},
			},
		}
		if err := repo.SaveAlbum(ctx, alb); err != nil {
			t.Fatalf("failed to save album: %v", err)
		}
	}

	// Enqueue all 4 albums
	for i := 1; i <= 4; i++ {
		jobID, err := orch.EnqueueAlbumDownload(fmt.Sprintf("alb-%d", i))
		if err != nil {
			t.Fatalf("failed to enqueue alb-%d: %v", i, err)
		}
		if jobID == "" {
			t.Fatalf("expected non-empty jobID for alb-%d", i)
		}
	}

	// Wait until all downloads complete
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, active, pending := orch.GetQueueStatus()
		if active == 0 && pending == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	maxSeen := dl.maxObserved.Load()
	if maxSeen != 2 {
		t.Errorf("expected max concurrent downloads of 2, observed %d", maxSeen)
	}

	max, active, pending := orch.GetQueueStatus()
	if max != 2 || active != 0 || pending != 0 {
		t.Errorf("expected idle queue (2, 0, 0), got (%d, %d, %d)", max, active, pending)
	}
}

func TestQueueManager_DynamicLimit(t *testing.T) {
	blockCh := make(chan struct{})
	dl := newMockDownloader(0, blockCh)
	orch, repo := setupTestOrchestrator(t, 1, dl)
	defer orch.queueMgr.Stop()

	ctx := context.Background()

	// Seed artist
	art := &domain.Artist{
		ID:        "art-dyn",
		Name:      "Dyn Artist",
		Source:    "deezer",
		SourceID:  "src-dyn",
		Monitored: true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.SaveArtist(ctx, art); err != nil {
		t.Fatalf("failed to save artist: %v", err)
	}

	for i := 1; i <= 3; i++ {
		alb := &domain.Album{
			ID:          fmt.Sprintf("dyn-alb-%d", i),
			ArtistID:    "art-dyn",
			SourceID:    fmt.Sprintf("dyn-src-%d", i),
			Title:       fmt.Sprintf("Dyn Album %d", i),
			ArtistName:  "Dyn Artist",
			ReleaseType: domain.ReleaseTypeAlbum,
			Status:      domain.DownloadStatus("wanted"),
			Tracks: []domain.Track{
				{ID: "t", Title: "T", DurationSec: 60, TrackNumber: 1},
			},
		}
		if err := repo.SaveAlbum(ctx, alb); err != nil {
			t.Fatalf("failed to save alb: %v", err)
		}
		if _, err := orch.EnqueueAlbumDownload(alb.ID); err != nil {
			t.Fatalf("failed to enqueue alb: %v", err)
		}
	}

	// Give time to start first download
	time.Sleep(30 * time.Millisecond)
	_, active, pending := orch.GetQueueStatus()
	if active != 1 || pending != 2 {
		t.Errorf("expected 1 active, 2 pending, got active=%d, pending=%d", active, pending)
	}

	// Expand concurrency to 3
	orch.queueMgr.SetMaxConcurrent(3)
	time.Sleep(40 * time.Millisecond)

	_, activeAfter, pendingAfter := orch.GetQueueStatus()
	if activeAfter != 3 || pendingAfter != 0 {
		t.Errorf("expected 3 active, 0 pending after expansion, got active=%d, pending=%d", activeAfter, pendingAfter)
	}

	// Release all
	close(blockCh)
}

func TestQueueManager_BatchEnqueueAndCancel(t *testing.T) {
	blockCh := make(chan struct{})
	dl := newMockDownloader(0, blockCh)
	orch, repo := setupTestOrchestrator(t, 1, dl)
	defer orch.queueMgr.Stop()

	ctx := context.Background()

	// Seed artist
	art := &domain.Artist{
		ID:        "art-batch",
		Name:      "Batch Artist",
		Source:    "deezer",
		SourceID:  "src-batch",
		Monitored: true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := repo.SaveArtist(ctx, art); err != nil {
		t.Fatalf("failed to save artist: %v", err)
	}

	ids := []string{"batch-1", "batch-2", "batch-3"}

	for _, id := range ids {
		alb := &domain.Album{
			ID:          id,
			ArtistID:    "art-batch",
			SourceID:    id,
			Title:       "Batch Album " + id,
			ArtistName:  "Batch Artist",
			ReleaseType: domain.ReleaseTypeAlbum,
			Status:      domain.DownloadStatus("wanted"),
			Tracks: []domain.Track{
				{ID: "t", Title: "T", DurationSec: 60, TrackNumber: 1},
			},
		}
		if err := repo.SaveAlbum(ctx, alb); err != nil {
			t.Fatalf("failed to save album %s: %v", id, err)
		}
	}

	count, err := orch.EnqueueBatchDownloads(ids)
	if err != nil {
		t.Fatalf("EnqueueBatchDownloads failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 enqueued, got %d", count)
	}

	time.Sleep(20 * time.Millisecond)

	// Cancel the pending batch-2
	if err := orch.CancelDownload("batch-2"); err != nil {
		t.Fatalf("CancelDownload failed: %v", err)
	}

	alb2, err := repo.GetAlbum(ctx, "batch-2")
	if err != nil || alb2 == nil {
		t.Fatalf("failed to retrieve alb2: %v", err)
	}
	if alb2.Status != domain.StatusFailed {
		t.Errorf("expected cancelled album status failed, got %s", alb2.Status)
	}

	// Unblock slot 1
	close(blockCh)
}
