package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/findr-app/findr/internal/domain"
)

func TestSQLiteRepository(t *testing.T) {
	tmpDB := "/tmp/findr_test.db"
	_ = os.Remove(tmpDB)
	defer os.Remove(tmpDB)

	repo, err := NewSQLiteRepository(tmpDB)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	defer repo.Close()

	ctx := context.Background()

	// 1. Test Artist persistence
	artist := &domain.Artist{
		ID:        "art-1",
		Name:      "Justice",
		Source:    "deezer",
		SourceID:  "123",
		Monitored: true,
	}

	if err := repo.SaveArtist(ctx, artist); err != nil {
		t.Fatalf("failed to save artist: %v", err)
	}

	fetchedArtist, err := repo.GetArtist(ctx, "art-1")
	if err != nil || fetchedArtist == nil {
		t.Fatalf("failed to get artist: %v", err)
	}
	if fetchedArtist.Name != "Justice" || !fetchedArtist.Monitored {
		t.Errorf("artist mismatch: %+v", fetchedArtist)
	}

	// 2. Test Album persistence
	album := &domain.Album{
		ID:          "alb-1",
		ArtistID:    "art-1",
		ArtistName:  "Justice",
		Title:       "Cross",
		ReleaseType: domain.ReleaseTypeAlbum,
		Year:        2007,
		TotalTracks: 2,
		Source:      "deezer",
		SourceID:    "456",
		Monitored:   true,
		Status:      domain.StatusPending,
		Tracks: []domain.Track{
			{ID: "t-1", DiscNumber: 1, TrackNumber: 1, Title: "Genesis", DurationSec: 234},
			{ID: "t-2", DiscNumber: 1, TrackNumber: 2, Title: "Let There Be Light", DurationSec: 295},
		},
	}

	if err := repo.SaveAlbum(ctx, album); err != nil {
		t.Fatalf("failed to save album: %v", err)
	}

	fetchedAlbum, err := repo.GetAlbum(ctx, "alb-1")
	if err != nil || fetchedAlbum == nil {
		t.Fatalf("failed to get album: %v", err)
	}
	if fetchedAlbum.Title != "Cross" || len(fetchedAlbum.Tracks) != 2 {
		t.Errorf("album mismatch: %+v", fetchedAlbum)
	}

	// 3. Test DownloadJob persistence
	job := &domain.DownloadJob{
		ID:            "job-1",
		AlbumID:       "alb-1",
		Downloader:    "qobuz",
		DownloadDir:   "/tmp/downloads",
		TargetQuality: domain.QualityFLAC16,
		Status:        domain.StatusDownloading,
		Progress:      45.0,
		CreatedAt:     time.Now(),
	}

	if err := repo.SaveDownloadJob(ctx, job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	fetchedJob, err := repo.GetDownloadJob(ctx, "job-1")
	if err != nil || fetchedJob == nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if fetchedJob.Progress != 45.0 || fetchedJob.Status != domain.StatusDownloading {
		t.Errorf("job mismatch: %+v", fetchedJob)
	}
}

func TestPlaylistRoundTrip(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "pl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	pl := &domain.Playlist{ID: "spotify-1", Source: "spotify", SourceID: "1", Name: "Mix", Monitored: true,
		Tracks:           []domain.PlaylistTrack{{Position: 1, Title: "A", Artists: []string{"X"}, AlbumID: "alb"}},
		ExcludedAlbumIDs: []string{"skip"}}
	if err := repo.SavePlaylist(ctx, pl); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetPlaylist(ctx, "spotify-1")
	if err != nil || got == nil || got.Name != "Mix" || len(got.Tracks) != 1 || got.Tracks[0].AlbumID != "alb" || got.ExcludedAlbumIDs[0] != "skip" || !got.Monitored {
		t.Fatalf("round trip failed: %+v %v", got, err)
	}
	list, _ := repo.ListPlaylists(ctx)
	if len(list) != 1 {
		t.Fatalf("list: %d", len(list))
	}
	_ = repo.DeletePlaylist(ctx, "spotify-1")
	if got, _ := repo.GetPlaylist(ctx, "spotify-1"); got != nil {
		t.Fatal("not deleted")
	}
}
