package importer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/findr-app/findr/internal/domain"
)

// Config configures library paths and renaming templates.
type Config struct {
	MusicPath    string // e.g. "/media/music"
	FolderFormat string // e.g. "{Artist}/{Album} ({Year})"
	FileFormat   string // e.g. "{Track:02d} - {Title}"
}

// Importer manages moving and renaming verified audio files into the music library.
type Importer struct {
	cfg        Config
	httpClient *http.Client
}

// NewImporter creates a new importer instance.
func NewImporter(cfg Config) *Importer {
	if cfg.FolderFormat == "" {
		cfg.FolderFormat = "{Artist}/{Album} ({Year})"
	}
	if cfg.FileFormat == "" {
		cfg.FileFormat = "{Track:02d} - {Title}"
	}
	return &Importer{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 30,
		},
	}
}

// ImportResult contains statistics and paths from the import operation.
type ImportResult struct {
	AlbumDir      string   `json:"album_dir"`
	ImportedFiles []string `json:"imported_files"`
	CoverArtPath  string   `json:"cover_art_path,omitempty"`
}

// ImportAlbum imports all verified files of an album into the canonical library location.
func (imp *Importer) ImportAlbum(ctx context.Context, album *domain.Album, match *domain.MatchResult) (*ImportResult, error) {
	if !match.Passed {
		return nil, fmt.Errorf("refusing to import unverified album: %s", match.Reason)
	}

	albumDir := imp.BuildAlbumPath(album)
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		return nil, fmt.Errorf("create album directory %s: %w", albumDir, err)
	}

	result := &ImportResult{
		AlbumDir:      albumDir,
		ImportedFiles: make([]string, 0, len(match.MatchedTracks)),
	}

	// 1. Move/copy each matched audio track
	for _, m := range match.MatchedTracks {
		ext := m.File.Extension
		destFilename := imp.BuildTrackFilename(album, m.Track, ext)
		destPath := filepath.Join(albumDir, destFilename)

		if err := moveOrCopyFile(m.File.Path, destPath); err != nil {
			return nil, fmt.Errorf("import file %s -> %s: %w", m.File.Path, destPath, err)
		}

		result.ImportedFiles = append(result.ImportedFiles, destPath)
	}

	// 2. Fetch and write high-res front cover art if available
	if album.CoverURL != "" {
		coverPath := filepath.Join(albumDir, "cover.jpg")
		if err := imp.downloadCover(ctx, album.CoverURL, coverPath); err == nil {
			result.CoverArtPath = coverPath
		}
	}

	return result, nil
}

// BuildAlbumPath generates destination folder path for the album.
func (imp *Importer) BuildAlbumPath(album *domain.Album) string {
	relFolder := imp.cfg.FolderFormat

	artist := sanitizePathSegment(album.ArtistName)
	albumTitle := sanitizePathSegment(album.Title)
	yearStr := ""
	if album.Year > 0 {
		yearStr = strconv.Itoa(album.Year)
	}

	relFolder = strings.ReplaceAll(relFolder, "{Artist}", artist)
	relFolder = strings.ReplaceAll(relFolder, "{Album}", albumTitle)
	relFolder = strings.ReplaceAll(relFolder, "{Year}", yearStr)

	// Clean up any empty parentheses if year is missing
	relFolder = strings.ReplaceAll(relFolder, " ()", "")

	return filepath.Join(imp.cfg.MusicPath, relFolder)
}

// BuildTrackFilename generates canonical filename for a track.
func (imp *Importer) BuildTrackFilename(album *domain.Album, track domain.Track, ext string) string {
	title := sanitizePathSegment(track.Title)
	artist := sanitizePathSegment(track.ArtistName)
	if artist == "" {
		artist = sanitizePathSegment(album.ArtistName)
	}

	trackStr := fmt.Sprintf("%02d", track.TrackNumber)
	discStr := fmt.Sprintf("%d", track.DiscNumber)

	filename := imp.cfg.FileFormat
	filename = strings.ReplaceAll(filename, "{Track:02d}", trackStr)
	filename = strings.ReplaceAll(filename, "{Track}", strconv.Itoa(track.TrackNumber))
	filename = strings.ReplaceAll(filename, "{Disc}", discStr)
	filename = strings.ReplaceAll(filename, "{Title}", title)
	filename = strings.ReplaceAll(filename, "{Artist}", artist)

	if track.DiscNumber > 1 && !strings.Contains(imp.cfg.FileFormat, "{Disc}") {
		filename = fmt.Sprintf("%d-%s", track.DiscNumber, filename)
	}

	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	return filename + ext
}

func (imp *Importer) downloadCover(ctx context.Context, imageURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return err
	}

	resp, err := imp.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cover download returned %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func moveOrCopyFile(src, dst string) error {
	// Attempt fast atomic rename first
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// Fallback to copy + remove across file systems/mounts
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

	in.Close()
	return os.Remove(src)
}

func sanitizePathSegment(name string) string {
	invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	res := name
	for _, c := range invalidChars {
		res = strings.ReplaceAll(res, c, "_")
	}
	return strings.TrimSpace(res)
}
