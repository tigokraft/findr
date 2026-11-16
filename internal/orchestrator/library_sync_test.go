package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/storage"
)

func TestParseAlbumFolder(t *testing.T) {
	tests := []struct {
		input     string
		wantTitle string
		wantYear  int
	}{
		{"Swimming (2018)", "Swimming", 2018},
		{"The Divine Feminine (2016)", "The Divine Feminine", 2016},
		{"SAVAGE MODE II (2020)", "SAVAGE MODE II", 2020},
		{"Album Without Year", "Album Without Year", 0},
		{"Title With (Parentheses) In Middle (2022)", "Title With (Parentheses) In Middle", 2022},
	}

	for _, tt := range tests {
		title, year := parseAlbumFolder(tt.input)
		if title != tt.wantTitle || year != tt.wantYear {
			t.Errorf("parseAlbumFolder(%q) = (%q, %d); want (%q, %d)", tt.input, title, year, tt.wantTitle, tt.wantYear)
		}
	}
}

func TestMatchAlbum(t *testing.T) {
	tests := []struct {
		folderTitle string
		folderYear  int
		albTitle    string
		albYear     int
		want        bool
	}{
		{"Swimming", 2018, "Swimming", 2018, true},
		{"GO-OD AM", 2015, "GO:OD AM", 2015, true},
		{"good kid, m.A.A.d city", 2012, "good kid, m.A.A.d city (Deluxe)", 2012, true},
		{"Savage Mode", 2016, "Savage Mode", 2017, true}, // within 1 year
		{"Different Album", 2018, "Swimming", 2018, false},
	}

	for _, tt := range tests {
		got := matchAlbum(tt.folderTitle, tt.folderYear, tt.albTitle, tt.albYear)
		if got != tt.want {
			t.Errorf("matchAlbum(%q, %d, %q, %d) = %v; want %v",
				tt.folderTitle, tt.folderYear, tt.albTitle, tt.albYear, got, tt.want)
		}
	}
}

func TestImportArtistsFromLibrary(t *testing.T) {
	tmpDir := t.TempDir()
	musicDir := filepath.Join(tmpDir, "music")
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create test artist folders with audio files
	artist1Dir := filepath.Join(musicDir, "Mac Miller", "Swimming (2018)")
	_ = os.MkdirAll(artist1Dir, 0755)
	_ = os.WriteFile(filepath.Join(artist1Dir, "01 - Come Back to Earth.flac"), []byte("dummy audio content"), 0644)

	artist2Dir := filepath.Join(musicDir, "Local Indie Band", "Demo (2024)")
	_ = os.MkdirAll(artist2Dir, 0755)
	_ = os.WriteFile(filepath.Join(artist2Dir, "01 - Intro.mp3"), []byte("dummy audio content"), 0644)

	repo, err := storage.NewSQLiteRepository(dbPath)
	if err != nil {
		t.Fatalf("failed to create repo: %v", err)
	}
	defer repo.Close()

	cfg := &config.Config{
		Library: config.LibraryConfig{
			MusicPath: musicDir,
		},
	}

	orch := NewOrchestrator(cfg, repo, nil, nil, nil, nil, nil)
	defer orch.StopQueue()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	summary, err := orch.ImportArtistsFromLibrary(ctx, nil)
	if err != nil {
		t.Fatalf("ImportArtistsFromLibrary failed: %v", err)
	}

	if summary.TotalFoldersFound != 2 {
		t.Errorf("TotalFoldersFound = %d; want 2", summary.TotalFoldersFound)
	}
	if summary.ArtistsImported != 2 {
		t.Errorf("ArtistsImported = %d; want 2", summary.ArtistsImported)
	}
	if summary.AlbumsMatched != 2 {
		t.Errorf("AlbumsMatched = %d; want 2", summary.AlbumsMatched)
	}

	// Verify artists in repo
	artists, err := repo.ListArtists(ctx)
	if err != nil {
		t.Fatalf("ListArtists failed: %v", err)
	}
	if len(artists) != 2 {
		t.Fatalf("Expected 2 artists in repo, got %d", len(artists))
	}

	// Run second time: should reconcile rather than re-import
	summary2, err := orch.ImportArtistsFromLibrary(ctx, nil)
	if err != nil {
		t.Fatalf("Second ImportArtistsFromLibrary failed: %v", err)
	}
	if summary2.ArtistsImported != 0 {
		t.Errorf("Expected 0 imported on re-run, got %d", summary2.ArtistsImported)
	}
	if summary2.ArtistsReconciled != 2 {
		t.Errorf("Expected 2 reconciled on re-run, got %d", summary2.ArtistsReconciled)
	}
}
