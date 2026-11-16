package importer

import (
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
