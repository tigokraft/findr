package downloader

import (
	"context"
	"errors"
	"testing"

	"github.com/findr-app/findr/internal/domain"
)

type mockDownloader struct {
	name          string
	searchResult  []SearchResult
	searchErr     error
	downloadJob   *domain.DownloadJob
	downloadErr   error
	trackPath     string
	trackErr      error
	downloadCalls int
	trackCalls    int
}

func (m *mockDownloader) Name() string {
	return m.name
}

func (m *mockDownloader) Search(ctx context.Context, album *domain.Album) ([]SearchResult, error) {
	return m.searchResult, m.searchErr
}

func (m *mockDownloader) Download(ctx context.Context, album *domain.Album, targetDir string, progress ProgressFunc) (*domain.DownloadJob, error) {
	m.downloadCalls++
	return m.downloadJob, m.downloadErr
}

func (m *mockDownloader) DownloadTrack(ctx context.Context, artist string, track domain.Track, destPath string, progress ProgressFunc) (string, error) {
	m.trackCalls++
	return m.trackPath, m.trackErr
}

func TestFallbackDownloader_DownloadCascadesOnPrimaryFailure(t *testing.T) {
	primary := &mockDownloader{
		name:        "primary",
		downloadErr: errors.New("album not found on primary"),
	}
	fallback := &mockDownloader{
		name: "fallback",
		downloadJob: &domain.DownloadJob{
			ID:         "job-fb",
			Downloader: "fallback",
			Status:     domain.StatusDownloaded,
		},
	}

	fbDL := NewFallbackDownloader(primary, fallback)

	album := &domain.Album{ID: "alb-1", ArtistName: "Artist", Title: "Title"}
	job, err := fbDL.Download(context.Background(), album, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("expected successful fallback, got error: %v", err)
	}

	if primary.downloadCalls != 1 {
		t.Errorf("expected 1 call to primary, got %d", primary.downloadCalls)
	}
	if fallback.downloadCalls != 1 {
		t.Errorf("expected 1 call to fallback, got %d", fallback.downloadCalls)
	}
	if job == nil || job.Downloader != "fallback" {
		t.Errorf("expected job from fallback downloader, got: %+v", job)
	}
}

func TestFallbackDownloader_DownloadPrimarySuccess(t *testing.T) {
	primary := &mockDownloader{
		name: "primary",
		downloadJob: &domain.DownloadJob{
			ID:         "job-primary",
			Downloader: "primary",
			Status:     domain.StatusDownloaded,
		},
	}
	fallback := &mockDownloader{
		name: "fallback",
	}

	fbDL := NewFallbackDownloader(primary, fallback)

	album := &domain.Album{ID: "alb-1", ArtistName: "Artist", Title: "Title"}
	job, err := fbDL.Download(context.Background(), album, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("expected successful primary download, got error: %v", err)
	}

	if primary.downloadCalls != 1 {
		t.Errorf("expected 1 call to primary, got %d", primary.downloadCalls)
	}
	if fallback.downloadCalls != 0 {
		t.Errorf("expected 0 calls to fallback, got %d", fallback.downloadCalls)
	}
	if job == nil || job.Downloader != "primary" {
		t.Errorf("expected job from primary downloader, got: %+v", job)
	}
}

func TestFallbackDownloader_DownloadTrackCascades(t *testing.T) {
	primary := &mockDownloader{
		name:     "primary",
		trackErr: errors.New("track unstreamable on primary"),
	}
	fallback := &mockDownloader{
		name:      "fallback",
		trackPath: "/tmp/fallback-track.flac",
	}

	fbDL := NewFallbackDownloader(primary, fallback)

	track := domain.Track{ID: "trk-1", TrackNumber: 1, Title: "Rare Song"}
	path, err := fbDL.DownloadTrack(context.Background(), "Artist", track, "/tmp/rare.flac", nil)
	if err != nil {
		t.Fatalf("expected successful track fallback, got error: %v", err)
	}

	if path != "/tmp/fallback-track.flac" {
		t.Errorf("expected path from fallback, got %s", path)
	}
	if primary.trackCalls != 1 {
		t.Errorf("expected 1 track call to primary, got %d", primary.trackCalls)
	}
	if fallback.trackCalls != 1 {
		t.Errorf("expected 1 track call to fallback, got %d", fallback.trackCalls)
	}
}
