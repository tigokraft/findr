package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/importer"
)

// ManualImportFile specifies a file to be imported and its metadata.
type ManualImportFile struct {
	SourcePath  string `json:"source_path"`
	FilePath    string `json:"file_path,omitempty"`
	TrackNumber int    `json:"track_number"`
	DiscNumber  int    `json:"disc_number"`
	Title       string `json:"title"`
	ArtistName  string `json:"artist_name,omitempty"`
}

// ManualImportRequest parameters for manual import.
type ManualImportRequest struct {
	ArtistID    string             `json:"artist_id"`
	ArtistName  string             `json:"artist_name"`
	AlbumID     string             `json:"album_id"`
	AlbumTitle  string             `json:"album_title"`
	ReleaseType domain.ReleaseType `json:"release_type"`
	Year        int                `json:"year"`
	CoverURL    string             `json:"cover_url,omitempty"`
	MoveOrCopy  string             `json:"move_or_copy"` // "move" or "copy"
	SourceDir   string             `json:"source_dir,omitempty"`
	Files       []ManualImportFile `json:"files"`
}

// ScanImportPath scans a directory for audio candidate files.
func (o *Orchestrator) ScanImportPath(ctx context.Context, path string) ([]domain.CandidateFile, error) {
	o.mu.RLock()
	vr := o.verifier
	o.mu.RUnlock()

	if vr == nil {
		return nil, fmt.Errorf("verifier not configured")
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat path %s: %w", path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %s is not a directory", path)
	}

	return vr.ScanCandidateFiles(path)
}

// ManualImport imports user-specified audio files into an album and artist in library.
func (o *Orchestrator) ManualImport(ctx context.Context, req ManualImportRequest) (*importer.ImportResult, error) {
	if len(req.Files) == 0 {
		return nil, fmt.Errorf("no files specified for manual import")
	}

	o.mu.RLock()
	imp := o.importer
	navi := o.navidrome
	cfg := o.cfg
	o.mu.RUnlock()

	if imp == nil {
		return nil, fmt.Errorf("importer not configured")
	}

	// 1. Resolve or create Artist
	var artist *domain.Artist
	var err error
	if req.ArtistID != "" {
		artist, err = o.repo.GetArtist(ctx, req.ArtistID)
		if err != nil {
			return nil, fmt.Errorf("retrieve artist %s: %w", req.ArtistID, err)
		}
	}

	if artist == nil && req.ArtistName != "" {
		// Search local library for existing artist by name
		artists, _ := o.repo.ListArtists(ctx)
		for _, a := range artists {
			if strings.EqualFold(a.Name, req.ArtistName) {
				artist = &a
				break
			}
		}
	}

	if artist == nil {
		artistName := req.ArtistName
		if artistName == "" {
			artistName = "Unknown Artist"
		}
		artID := fmt.Sprintf("art-manual-%d", time.Now().UnixNano())
		artist = &domain.Artist{
			ID:        artID,
			Name:      artistName,
			Source:    "manual",
			SourceID:  artID,
			Monitored: true,
			CreatedAt: time.Now(),
		}
		if err := o.repo.SaveArtist(ctx, artist); err != nil {
			return nil, fmt.Errorf("save manual artist: %w", err)
		}
	}

	// 2. Resolve or create Album
	var album *domain.Album
	if req.AlbumID != "" {
		album, err = o.repo.GetAlbum(ctx, req.AlbumID)
		if err != nil {
			return nil, fmt.Errorf("retrieve album %s: %w", req.AlbumID, err)
		}
	}

	if album == nil {
		albumTitle := req.AlbumTitle
		if albumTitle == "" {
			albumTitle = "Unknown Album"
		}
		albID := fmt.Sprintf("alb-manual-%d", time.Now().UnixNano())
		relType := req.ReleaseType
		if relType == "" {
			relType = domain.ReleaseTypeAlbum
		}
		album = &domain.Album{
			ID:          albID,
			ArtistID:    artist.ID,
			ArtistName:  artist.Name,
			Title:       albumTitle,
			ReleaseType: relType,
			Year:        req.Year,
			CoverURL:    req.CoverURL,
			Source:      "manual",
			SourceID:    albID,
			Monitored:   true,
			Status:      domain.StatusCompleted,
			CreatedAt:   time.Now(),
		}
	}

	// 3. Prepare tracks & track files
	tracks := make([]domain.Track, 0, len(req.Files))
	trackImports := make([]importer.ManualTrackImport, 0, len(req.Files))

	for i, f := range req.Files {
		src := f.SourcePath
		if src == "" {
			src = f.FilePath
		}
		if src == "" {
			continue
		}

		tNum := f.TrackNumber
		if tNum <= 0 {
			tNum = i + 1
		}
		dNum := f.DiscNumber
		if dNum <= 0 {
			dNum = 1
		}
		title := f.Title
		if title == "" {
			base := filepath.Base(src)
			ext := filepath.Ext(base)
			title = strings.TrimSuffix(base, ext)
		}
		artName := f.ArtistName
		if artName == "" {
			artName = artist.Name
		}

		trk := domain.Track{
			ID:          fmt.Sprintf("trk-%s-%d", album.ID, i+1),
			AlbumID:     album.ID,
			ArtistName:  artName,
			Title:       title,
			DiscNumber:  dNum,
			TrackNumber: tNum,
			FilePath:    src,
		}
		tracks = append(tracks, trk)

		trackImports = append(trackImports, importer.ManualTrackImport{
			SourcePath:  src,
			TrackNumber: tNum,
			DiscNumber:  dNum,
			Title:       title,
			ArtistName:  artName,
		})
	}

	album.Tracks = tracks
	album.TotalTracks = len(tracks)
	album.Status = domain.StatusCompleted

	// 4. Perform file import
	moveOrCopy := req.MoveOrCopy
	if moveOrCopy == "" {
		moveOrCopy = cfg.Import.MoveOrCopy
	}
	result, err := imp.ImportManual(ctx, album, trackImports, moveOrCopy)
	if err != nil {
		return nil, fmt.Errorf("manual import files: %w", err)
	}

	// Ensure artist artwork (artist.jpg) is present in the artist folder for Navidrome/media servers
	if imp != nil && result != nil && result.AlbumDir != "" {
		artistDir := filepath.Dir(result.AlbumDir)
		artistJpg := filepath.Join(artistDir, "artist.jpg")
		if _, statErr := os.Stat(artistJpg); os.IsNotExist(statErr) {
			if artist != nil && artist.ImageURL != "" {
				_ = imp.DownloadImage(ctx, artist.ImageURL, artistJpg)
			}
		}
	}

	// 5. Save updated album to database
	if err := o.repo.SaveAlbum(ctx, album); err != nil {
		return nil, fmt.Errorf("save imported album: %w", err)
	}

	// 6. Record completed job in download_jobs history
	now := time.Now()
	jobID := fmt.Sprintf("import-%s-%d", album.ID, now.Unix())
	job := &domain.DownloadJob{
		ID:              jobID,
		AlbumID:         album.ID,
		Downloader:      "manual_import",
		DownloadDir:     result.AlbumDir,
		TargetQuality:   domain.QualityFLAC16,
		Status:          domain.StatusCompleted,
		Progress:        100.0,
		DownloadedFiles: result.ImportedFiles,
		CreatedAt:       now,
		CompletedAt:     &now,
	}
	_ = o.repo.SaveDownloadJob(ctx, job)

	// 7. Trigger Navidrome scan if enabled
	if cfg.Navidrome.Enabled && navi != nil {
		_, _ = navi.StartScan(ctx, false)
	}

	// 8. Clean up temporary staging directory if instructed
	if req.SourceDir != "" && strings.Contains(req.SourceDir, "manual_uploads") {
		_ = os.RemoveAll(req.SourceDir)
	}

	fmt.Printf("[findr Manual Import] Successfully imported '%s - %s' (%d tracks) into %s\n",
		artist.Name, album.Title, len(result.ImportedFiles), result.AlbumDir)

	return result, nil
}
