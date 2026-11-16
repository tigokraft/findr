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
	"time"

	"github.com/findr-app/findr/internal/domain"
)

// Config configures library paths and renaming templates.
type Config struct {
	MusicPath         string
	FolderFormat      string
	FileFormat        string
	DeleteAfterImport bool
	MoveOrCopy        string // "move" or "copy"
	CleanEmptyFolders bool
	DownloadCoverArt  bool
	CoverFilename     string // default: "cover.jpg"
	WriteNFO          bool
	GenerateM3U       bool
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
	if cfg.CoverFilename == "" {
		cfg.CoverFilename = "cover.jpg"
	}
	return &Importer{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ImportResult contains statistics and paths from the import operation.
type ImportResult struct {
	AlbumDir      string   `json:"album_dir"`
	ImportedFiles []string `json:"imported_files"`
	CoverArtPath  string   `json:"cover_art_path,omitempty"`
	NFOPath       string   `json:"nfo_path,omitempty"`
	M3UPath       string   `json:"m3u_path,omitempty"`
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

		var err error
		if imp.cfg.MoveOrCopy == "copy" {
			err = copyOnlyFile(m.File.Path, destPath)
		} else {
			err = moveOrCopyFile(m.File.Path, destPath)
		}

		if err != nil {
			return nil, fmt.Errorf("import file %s -> %s: %w", m.File.Path, destPath, err)
		}

		if strings.ToLower(filepath.Ext(destPath)) == ".flac" {
			_ = TagFLAC(destPath, TrackTags{
				Title:       m.Track.Title,
				Artist:      m.Track.ArtistName,
				Album:       album.Title,
				AlbumArtist: album.ArtistName,
				TrackNumber: m.Track.TrackNumber,
				DiscNumber:  m.Track.DiscNumber,
				Year:        album.Year,
				ISRC:        m.Track.ISRC,
			})
		}

		result.ImportedFiles = append(result.ImportedFiles, destPath)
	}

	// 2. Fetch and write high-res front cover art if configured
	if imp.cfg.DownloadCoverArt && album.CoverURL != "" {
		coverName := imp.cfg.CoverFilename
		if coverName == "" {
			coverName = "cover.jpg"
		}
		coverPath := filepath.Join(albumDir, coverName)
		if err := imp.DownloadImage(ctx, album.CoverURL, coverPath); err == nil {
			result.CoverArtPath = coverPath
		} else {
			fmt.Printf("[importer Warning] Failed to download cover art from %s to %s: %v\n", album.CoverURL, coverPath, err)
		}
	}

	// 3. Write Kodi/Plex compatible album.nfo metadata if configured
	if imp.cfg.WriteNFO {
		nfoPath := filepath.Join(albumDir, "album.nfo")
		if err := writeNFOFile(nfoPath, album); err == nil {
			result.NFOPath = nfoPath
		}
	}

	// 4. Generate M3U playlist if configured
	if imp.cfg.GenerateM3U && len(result.ImportedFiles) > 0 {
		m3uPath := filepath.Join(albumDir, "playlist.m3u")
		if err := writeM3UFile(m3uPath, result.ImportedFiles); err == nil {
			result.M3UPath = m3uPath
		}
	}

	return result, nil
}

// ManualTrackImport represents a single track file to be imported manually.
type ManualTrackImport struct {
	SourcePath  string
	TrackNumber int
	DiscNumber  int
	Title       string
	ArtistName  string
}

// ImportManual imports files into canonical library structure without requiring a full MatchResult.
func (imp *Importer) ImportManual(ctx context.Context, album *domain.Album, files []ManualTrackImport, moveOrCopy string) (*ImportResult, error) {
	if moveOrCopy == "" {
		moveOrCopy = imp.cfg.MoveOrCopy
	}
	if moveOrCopy == "" {
		moveOrCopy = "move"
	}

	albumDir := imp.BuildAlbumPath(album)
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		return nil, fmt.Errorf("create album directory %s: %w", albumDir, err)
	}

	result := &ImportResult{
		AlbumDir:      albumDir,
		ImportedFiles: make([]string, 0, len(files)),
	}

	for i, f := range files {
		trackNum := f.TrackNumber
		if trackNum <= 0 {
			trackNum = i + 1
		}
		discNum := f.DiscNumber
		if discNum <= 0 {
			discNum = 1
		}
		title := f.Title
		if title == "" {
			base := filepath.Base(f.SourcePath)
			ext := filepath.Ext(base)
			title = strings.TrimSuffix(base, ext)
		}
		trackArtist := f.ArtistName
		if trackArtist == "" {
			trackArtist = album.ArtistName
		}

		track := domain.Track{
			ID:          fmt.Sprintf("trk-%d", i+1),
			AlbumID:     album.ID,
			ArtistName:  trackArtist,
			Title:       title,
			DiscNumber:  discNum,
			TrackNumber: trackNum,
		}

		ext := filepath.Ext(f.SourcePath)
		destFilename := imp.BuildTrackFilename(album, track, ext)
		destPath := filepath.Join(albumDir, destFilename)

		var err error
		if moveOrCopy == "copy" {
			err = copyOnlyFile(f.SourcePath, destPath)
		} else {
			err = moveOrCopyFile(f.SourcePath, destPath)
		}
		if err != nil {
			return nil, fmt.Errorf("import file %s -> %s: %w", f.SourcePath, destPath, err)
		}

		if strings.ToLower(filepath.Ext(destPath)) == ".flac" {
			_ = TagFLAC(destPath, TrackTags{
				Title:       track.Title,
				Artist:      track.ArtistName,
				Album:       album.Title,
				AlbumArtist: album.ArtistName,
				TrackNumber: track.TrackNumber,
				DiscNumber:  track.DiscNumber,
				Year:        album.Year,
			})
		}

		result.ImportedFiles = append(result.ImportedFiles, destPath)
	}

	// High-res cover art if available
	if imp.cfg.DownloadCoverArt && album.CoverURL != "" {
		coverName := imp.cfg.CoverFilename
		if coverName == "" {
			coverName = "cover.jpg"
		}
		coverPath := filepath.Join(albumDir, coverName)
		if err := imp.DownloadImage(ctx, album.CoverURL, coverPath); err == nil {
			result.CoverArtPath = coverPath
		} else {
			fmt.Printf("[importer Warning] Failed to download cover art from %s to %s: %v\n", album.CoverURL, coverPath, err)
		}
	}

	// Write NFO if enabled
	if imp.cfg.WriteNFO {
		nfoPath := filepath.Join(albumDir, "album.nfo")
		if err := writeNFOFile(nfoPath, album); err == nil {
			result.NFOPath = nfoPath
		}
	}

	// Generate M3U playlist if enabled
	if imp.cfg.GenerateM3U && len(result.ImportedFiles) > 0 {
		m3uPath := filepath.Join(albumDir, "playlist.m3u")
		if err := writeM3UFile(m3uPath, result.ImportedFiles); err == nil {
			result.M3UPath = m3uPath
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

// DownloadImage fetches an image from imageURL and saves it to destPath.
func (imp *Importer) DownloadImage(ctx context.Context, imageURL, destPath string) error {
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
		return fmt.Errorf("image download returned %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create image directory: %w", err)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func (imp *Importer) downloadCover(ctx context.Context, imageURL, destPath string) error {
	return imp.DownloadImage(ctx, imageURL, destPath)
}

func writeNFOFile(destPath string, album *domain.Album) error {
	nfo := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>
<album>
    <title>%s</title>
    <artist>%s</artist>
    <year>%d</year>
    <releasedate>%s</releasedate>
    <type>%s</type>
    <tracks>%d</tracks>
    <upc>%s</upc>
    <source>%s</source>
    <sourceid>%s</sourceid>
</album>
`, album.Title, album.ArtistName, album.Year, album.ReleaseDate, string(album.ReleaseType), album.TotalTracks, album.UPC, album.Source, album.SourceID)

	return os.WriteFile(destPath, []byte(nfo), 0644)
}

func writeM3UFile(destPath string, importedFiles []string) error {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	for _, f := range importedFiles {
		sb.WriteString(filepath.Base(f))
		sb.WriteString("\n")
	}
	return os.WriteFile(destPath, []byte(sb.String()), 0644)
}

func copyOnlyFile(src, dst string) error {
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

	_, err = io.Copy(out, in)
	return err
}

func moveOrCopyFile(src, dst string) error {
	// Attempt fast atomic rename first
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// Fallback to copy + remove across file systems/mounts
	if err := copyOnlyFile(src, dst); err != nil {
		return err
	}

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
