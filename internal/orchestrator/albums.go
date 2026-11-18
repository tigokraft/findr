package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/verifier"
)

// AlbumSearchResult is an indexer release annotated with its library state.
type AlbumSearchResult struct {
	domain.Album
	InLibrary bool `json:"in_library"`
}

// SearchAlbums searches the active indexer for releases by title.
func (o *Orchestrator) SearchAlbums(ctx context.Context, source, query string) ([]AlbumSearchResult, error) {
	idx := o.indexerFor(source)

	searcher, ok := idx.(indexer.AlbumSearcher)
	if !ok {
		return nil, fmt.Errorf("the %s indexer does not support album search", idx.Name())
	}
	albums, err := searcher.SearchAlbum(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search albums '%s': %w", query, err)
	}

	results := make([]AlbumSearchResult, 0, len(albums))
	for _, a := range albums {
		r := AlbumSearchResult{Album: a}
		if r.Source == "" {
			r.Source = idx.Name()
		}
		if existing, err := o.lookupAlbum(ctx, r.Source, r.SourceID); err == nil && existing != nil {
			r.InLibrary = true
			r.WantedTrackIDs = existing.WantedTrackIDs
			r.Status = existing.Status
			r.Monitored = existing.Monitored
			r.ArtistID = existing.ArtistID
			if r.Year == 0 {
				r.Year = existing.Year
			}
		}
		results = append(results, r)
	}
	return results, nil
}

// AddAlbum tracks a whole release from the default indexer. See AddRelease.
func (o *Orchestrator) AddAlbum(ctx context.Context, sourceID string, download bool) (*domain.Album, error) {
	return o.AddRelease(ctx, AddReleaseRequest{SourceID: sourceID, Download: download})
}

// AddRelease tracks a single release, or only some of its tracks, without
// following the rest of the artist's discography. The artist is created
// unmonitored if it is not in the library yet, so the scheduler will not index
// their other releases. When Download is true and the wanted tracks are not
// already on disk the release is queued.
func (o *Orchestrator) AddRelease(ctx context.Context, req AddReleaseRequest) (*domain.Album, error) {
	sourceID := strings.TrimSpace(req.SourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("album id is required")
	}
	idx := o.indexerFor(req.Source)
	source := strings.ToLower(strings.TrimSpace(req.Source))
	if source == "" {
		source = idx.Name()
	}
	picked := dedupe(req.TrackIDs)

	album, err := o.lookupAlbum(ctx, source, sourceID)
	if err != nil {
		return nil, fmt.Errorf("look up album %s: %w", sourceID, err)
	}
	if album == nil && source == idx.Name() {
		// Records saved before catalog-qualified IDs used the bare ID.
		if a, err := o.repo.GetAlbum(ctx, sourceID); err == nil && a != nil {
			album = a
		}
	}

	needsDownload := false
	if album == nil {
		detailed, err := idx.GetAlbumDetails(ctx, sourceID)
		if err != nil {
			return nil, fmt.Errorf("fetch album %s from %s: %w", sourceID, idx.Name(), err)
		}
		if detailed == nil {
			return nil, fmt.Errorf("album %s not found on %s", sourceID, idx.Name())
		}
		if detailed.Source == "" {
			detailed.Source = idx.Name()
		}
		if detailed.SourceID == "" {
			detailed.SourceID = sourceID
		}
		detailed.ID = o.libraryID(detailed.Source, detailed.SourceID)
		artist, err := o.ensureArtistForAlbum(ctx, idx, detailed)
		if err != nil {
			return nil, err
		}
		detailed.ArtistID = artist.ID
		if artist.Name != "" {
			detailed.ArtistName = artist.Name
		}
		normalizeTracks(detailed)
		detailed.Monitored = true
		detailed.Status = domain.StatusPending
		if len(picked) > 0 && len(picked) < len(detailed.Tracks) {
			detailed.WantedTrackIDs = picked
		}
		album = detailed
		needsDownload = true
	} else {
		album.Monitored = true
		if len(album.Tracks) == 0 && len(picked) > 0 {
			if full, err := o.EnsureAlbumTracks(ctx, album.ID); err == nil && full != nil {
				album = full
				album.Monitored = true
			}
		}
		wanted, changed := mergeWanted(album, picked)
		album.WantedTrackIDs = wanted
		needsDownload = album.Status != domain.StatusCompleted
		if changed && album.Status == domain.StatusCompleted {
			// New tracks were picked for a release that is already on disk.
			album.Status = domain.StatusPending
			needsDownload = true
		}
	}

	if err := o.repo.SaveAlbum(ctx, album); err != nil {
		return nil, fmt.Errorf("save album: %w", err)
	}

	if req.Download && needsDownload {
		if _, err := o.EnqueueAlbumDownload(album.ID); err != nil {
			return album, fmt.Errorf("queue download: %w", err)
		}
		album.Status = domain.StatusPending
	}
	return album, nil
}

// ensureArtistForAlbum returns the library artist for an album, creating an
// unmonitored entry when the artist is new.
func (o *Orchestrator) ensureArtistForAlbum(ctx context.Context, idx indexer.Indexer, album *domain.Album) (*domain.Artist, error) {
	if album.ArtistID != "" {
		if a, err := o.lookupArtist(ctx, idx.Name(), album.ArtistID); err == nil && a != nil {
			return a, nil
		}
	}
	if album.ArtistName != "" {
		if all, err := o.repo.ListArtists(ctx); err == nil {
			for i := range all {
				if strings.EqualFold(all[i].Name, album.ArtistName) {
					return &all[i], nil
				}
			}
		}
	}

	artist := &domain.Artist{
		ID:       o.libraryID(idx.Name(), album.ArtistID),
		SourceID: album.ArtistID,
		Name:     album.ArtistName,
		Source:   idx.Name(),
	}
	if album.ArtistID != "" {
		if fetched, err := idx.GetArtist(ctx, album.ArtistID); err == nil && fetched != nil {
			artist.ImageURL = fetched.ImageURL
			if artist.Name == "" {
				artist.Name = fetched.Name
			}
			if fetched.Source != "" {
				artist.Source = fetched.Source
			}
		}
	}
	if album.ArtistID == "" {
		artist.ID = "artist-" + normalizeKey(artist.Name)
		artist.SourceID = artist.ID
	}
	if artist.Name == "" {
		return nil, fmt.Errorf("album %s has no artist", album.ID)
	}
	artist.Monitored = false // album-only: do not follow the whole discography
	if err := o.repo.SaveArtist(ctx, artist); err != nil {
		return nil, fmt.Errorf("save artist: %w", err)
	}
	return artist, nil
}

func normalizeTracks(album *domain.Album) {
	for i := range album.Tracks {
		if album.Tracks[i].TrackNumber <= 0 {
			album.Tracks[i].TrackNumber = i + 1
		}
		if album.Tracks[i].DiscNumber <= 0 {
			album.Tracks[i].DiscNumber = 1
		}
		if album.Tracks[i].ArtistName == "" {
			album.Tracks[i].ArtistName = album.ArtistName
		}
	}
	if album.TotalTracks == 0 {
		album.TotalTracks = len(album.Tracks)
	}
}

// RemoveAlbum deletes a release from the database. Files on disk are untouched.
func (o *Orchestrator) RemoveAlbum(ctx context.Context, albumID string) error {
	_ = o.CancelDownload(albumID)
	return o.repo.DeleteAlbum(ctx, albumID)
}

// MarkAlbumFilesRemoved resets a release after its files were deleted from disk.
func (o *Orchestrator) MarkAlbumFilesRemoved(ctx context.Context, albumID string, unmonitor bool) (*domain.Album, error) {
	album, err := o.repo.GetAlbum(ctx, albumID)
	if err != nil || album == nil {
		return nil, fmt.Errorf("album %s not found", albumID)
	}
	album.Status = domain.StatusPending
	if unmonitor {
		album.Monitored = false
	}
	if err := o.repo.SaveAlbum(ctx, album); err != nil {
		return nil, err
	}
	return album, nil
}

// MusicPath returns the configured library root.
func (o *Orchestrator) MusicPath() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.cfg == nil {
		return ""
	}
	return o.cfg.Library.MusicPath
}

// LocateAlbumDir finds the folder holding a release on disk. It first tries the
// path the importer would build, then scans the artist folder for a folder whose
// name matches the release title (for albums imported under other naming).
func (o *Orchestrator) LocateAlbumDir(ctx context.Context, albumID string) (*domain.Album, string, error) {
	album, err := o.repo.GetAlbum(ctx, albumID)
	if err != nil || album == nil {
		return nil, "", fmt.Errorf("album %s not found", albumID)
	}
	dir := o.locateAlbumDir(album)
	return album, dir, nil
}

func (o *Orchestrator) locateAlbumDir(album *domain.Album) string {
	o.mu.RLock()
	imp := o.importer
	musicPath := ""
	if o.cfg != nil {
		musicPath = o.cfg.Library.MusicPath
	}
	o.mu.RUnlock()

	var artistDirs []string
	if imp != nil {
		expected := imp.BuildAlbumPath(album)
		if isDir(expected) {
			return expected
		}
		artistDirs = append(artistDirs, filepath.Dir(expected))
	}
	if musicPath != "" {
		// Artist folders whose name matches loosely (case, punctuation, accents kept).
		if entries, err := os.ReadDir(musicPath); err == nil {
			want := strings.ToLower(strings.TrimSpace(album.ArtistName))
			for _, e := range entries {
				if e.IsDir() && verifier.StringSimilarity(strings.ToLower(e.Name()), want) >= 0.95 {
					artistDirs = append(artistDirs, filepath.Join(musicPath, e.Name()))
				}
			}
		}
	}

	seen := map[string]bool{}
	for _, ad := range artistDirs {
		if seen[ad] || !isDir(ad) {
			continue
		}
		seen[ad] = true
		entries, err := os.ReadDir(ad)
		if err != nil {
			continue
		}
		best, bestScore := "", 0.0
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			title, year := parseAlbumFolder(e.Name())
			if matchAlbum(title, year, album.Title, album.Year) {
				return filepath.Join(ad, e.Name())
			}
			if s := verifier.StringSimilarity(title, album.Title); s > bestScore {
				best, bestScore = e.Name(), s
			}
		}
		if bestScore >= 0.9 {
			return filepath.Join(ad, best)
		}
	}
	return ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// MarkDeletedPath resets releases whose files lived in a folder that was just
// deleted from the music library (an album folder or a whole artist folder), so
// they show as missing again instead of downloaded.
func (o *Orchestrator) MarkDeletedPath(ctx context.Context, absDir string) int {
	musicPath := o.MusicPath()
	if musicPath == "" || !within(musicPath, absDir) {
		return 0
	}
	rel, err := filepath.Rel(filepath.Clean(musicPath), filepath.Clean(absDir))
	if err != nil || rel == "." {
		return 0
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	artistFolder := parts[0]

	artists, err := o.repo.ListArtists(ctx)
	if err != nil {
		return 0
	}
	marked := 0
	for _, ar := range artists {
		if verifier.StringSimilarity(strings.ToLower(ar.Name), strings.ToLower(artistFolder)) < 0.95 {
			continue
		}
		albums, err := o.repo.ListAlbumsByArtist(ctx, ar.ID)
		if err != nil {
			continue
		}
		for i := range albums {
			a := &albums[i]
			if a.Status != domain.StatusCompleted {
				continue
			}
			hit := len(parts) == 1 // whole artist folder removed
			if !hit {
				title, year := parseAlbumFolder(parts[1])
				hit = matchAlbum(title, year, a.Title, a.Year) || verifier.StringSimilarity(title, a.Title) >= 0.9
			}
			if !hit {
				continue
			}
			if a.Source == "local" {
				_ = o.repo.DeleteAlbum(ctx, a.ID) // folder-only record; nothing left to track
			} else {
				a.Status = domain.StatusPending
				_ = o.repo.SaveAlbum(ctx, a)
			}
			marked++
		}
	}
	return marked
}
