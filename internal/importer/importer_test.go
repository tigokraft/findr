package importer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/findr-app/findr/internal/domain"
)

func TestBuildAlbumPath(t *testing.T) {
	imp := NewImporter(Config{
		MusicPath:    "/music",
		FolderFormat: "{Artist}/{Album} ({Year})",
		FileFormat:   "{Track:02d} - {Title}",
	})

	album := &domain.Album{
		ArtistName: "AC/DC",
		Title:      "Back In Black",
		Year:       1980,
	}

	path := imp.BuildAlbumPath(album)
	expected := "/music/AC_DC/Back In Black (1980)"
	if path != expected {
		t.Errorf("BuildAlbumPath() = %q; expected %q", path, expected)
	}
}

func TestBuildTrackFilename(t *testing.T) {
	imp := NewImporter(Config{
		MusicPath:    "/music",
		FolderFormat: "{Artist}/{Album} ({Year})",
		FileFormat:   "{Track:02d} - {Title}",
	})

	album := &domain.Album{
		ArtistName: "Daft Punk",
	}
	track := domain.Track{
		TrackNumber: 3,
		DiscNumber:  1,
		Title:       "Digital Love?",
	}

	filename := imp.BuildTrackFilename(album, track, ".flac")
	expected := "03 - Digital Love_.flac"
	if filename != expected {
		t.Errorf("BuildTrackFilename() = %q; expected %q", filename, expected)
	}
}

func TestDownloadImage(t *testing.T) {
	expectedData := []byte("fake-jpeg-image-data")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(expectedData)
	}))
	defer ts.Close()

	imp := NewImporter(Config{})
	tmpDir := t.TempDir()
	dest := filepath.Join(tmpDir, "subfolder", "cover.jpg")

	err := imp.DownloadImage(context.Background(), ts.URL, dest)
	if err != nil {
		t.Fatalf("DownloadImage failed: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(expectedData) {
		t.Errorf("got %q, want %q", got, expectedData)
	}
}
