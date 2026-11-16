package orchestrator

import (
	"context"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/findr-app/findr/internal/domain"
)

var yearSuffixRegex = regexp.MustCompile(`\s*\((\d{4})\)\s*$`)

// LibrarySyncSummary records the results of importing/reconciling artists from the library path.
type LibrarySyncSummary struct {
	TotalFoldersFound int      `json:"total_folders_found"`
	ArtistsImported   int      `json:"artists_imported"`
	ArtistsReconciled int      `json:"artists_reconciled"`
	AlbumsMatched     int      `json:"albums_matched"`
	Errors            []string `json:"errors,omitempty"`
}

// ImportArtistsFromLibrary scans the configured MusicPath, discovers existing artist folders,
// imports missing artists into the database with full discography indexing, and reconciles
// all existing albums on disk as completed/downloaded.
func (o *Orchestrator) ImportArtistsFromLibrary(ctx context.Context, progressFn func(current, total int, artist string)) (*LibrarySyncSummary, error) {
	o.mu.RLock()
	musicPath := o.cfg.Library.MusicPath
	idx := o.indexer
	o.mu.RUnlock()

	if strings.TrimSpace(musicPath) == "" {
		return nil, fmt.Errorf("no library music_path configured")
	}

	info, err := os.Stat(musicPath)
	if err != nil {
		return nil, fmt.Errorf("library music_path '%s': %w", musicPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("library music_path '%s' is not a directory", musicPath)
	}

	entries, err := os.ReadDir(musicPath)
	if err != nil {
		return nil, fmt.Errorf("read library music_path '%s': %w", musicPath, err)
	}

	// Filter directories
	var artistDirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || isIgnoredDir(name) {
			continue
		}
		artistDirs = append(artistDirs, name)
	}

	summary := &LibrarySyncSummary{
		TotalFoldersFound: len(artistDirs),
	}

	// Fetch existing artists from repo for quick deduplication
	existingArtists, err := o.repo.ListArtists(ctx)
	if err != nil {
		return nil, fmt.Errorf("list existing artists: %w", err)
	}
	existingMap := make(map[string]*domain.Artist)
	for i := range existingArtists {
		a := &existingArtists[i]
		existingMap[normalizeKey(a.Name)] = a
	}

	for i, artistName := range artistDirs {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		default:
		}

		if progressFn != nil {
			progressFn(i+1, len(artistDirs), artistName)
		}

		artistFullPath := filepath.Join(musicPath, artistName)
		normKey := normalizeKey(artistName)
		existing, found := existingMap[normKey]

		if found {
			// Already tracked: reconcile albums on disk
			matched, err := o.reconcileArtistAlbums(ctx, existing, artistFullPath)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("%s reconcile: %v", artistName, err))
			}
			summary.AlbumsMatched += matched
			summary.ArtistsReconciled++
			continue
		}

		// Artist not yet in findr: search indexer
		var matchedArtist *domain.Artist
		if idx != nil {
			searchResults, err := idx.SearchArtist(ctx, artistName)
			if err == nil && len(searchResults) > 0 {
				// Look for an exact (normalized) match first
				for _, res := range searchResults {
					if normalizeKey(res.Name) == normKey {
						matchedArtist = &res
						break
					}
				}
				// Otherwise fallback to first search result
				if matchedArtist == nil && len(searchResults) > 0 {
					matchedArtist = &searchResults[0]
				}
			}
			// Rate limiting delay between API requests
			time.Sleep(120 * time.Millisecond)
		}

		if matchedArtist != nil {
			savedArtist, _, err := o.AddArtist(ctx, matchedArtist, true, false)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("add artist '%s': %v", artistName, err))
				continue
			}
			existingMap[normKey] = savedArtist
			summary.ArtistsImported++

			// Reconcile albums against disk
			matched, err := o.reconcileArtistAlbums(ctx, savedArtist, artistFullPath)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("%s reconcile: %v", artistName, err))
			}
			summary.AlbumsMatched += matched
		} else {
			// Fallback: create local artist entry if indexer yielded no results
			localID := fmt.Sprintf("local-%x", md5.Sum([]byte(strings.ToLower(artistName))))[:16]
			now := time.Now()
			localArtist := &domain.Artist{
				ID:        localID,
				Name:      artistName,
				Source:    "local",
				SourceID:  localID,
				Monitored: true,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := o.repo.SaveArtist(ctx, localArtist); err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("save local artist '%s': %v", artistName, err))
				continue
			}
			existingMap[normKey] = localArtist
			summary.ArtistsImported++

			matched, err := o.reconcileArtistAlbums(ctx, localArtist, artistFullPath)
			if err != nil {
				summary.Errors = append(summary.Errors, fmt.Sprintf("%s reconcile: %v", artistName, err))
			}
			summary.AlbumsMatched += matched
		}
	}

	return summary, nil
}

// reconcileArtistAlbums inspects the artist folder on disk, compares album folders with
// indexed albums, and marks matching albums as domain.StatusCompleted. If unindexed album
// folders exist on disk, it creates local completed album records for them.
func (o *Orchestrator) reconcileArtistAlbums(ctx context.Context, artist *domain.Artist, artistDir string) (int, error) {
	subEntries, err := os.ReadDir(artistDir)
	if err != nil {
		return 0, err
	}

	var diskFolders []string
	for _, sub := range subEntries {
		if !sub.IsDir() {
			continue
		}
		name := sub.Name()
		if strings.HasPrefix(name, ".") || isIgnoredDir(name) {
			continue
		}
		diskFolders = append(diskFolders, name)
	}

	if len(diskFolders) == 0 {
		return 0, nil
	}

	existingAlbums, err := o.repo.ListAlbumsByArtist(ctx, artist.ID)
	if err != nil {
		return 0, err
	}

	matchedCount := 0
	matchedDiskFolders := make(map[string]bool)

	// Pass 1: Match disk folders against indexed albums
	for _, folderName := range diskFolders {
		folderPath := filepath.Join(artistDir, folderName)
		if !dirHasAudioFiles(folderPath) {
			continue
		}

		folderTitle, folderYear := parseAlbumFolder(folderName)

		for i := range existingAlbums {
			alb := &existingAlbums[i]
			if matchAlbum(folderTitle, folderYear, alb.Title, alb.Year) {
				matchedDiskFolders[folderName] = true
				if alb.Status != domain.StatusCompleted {
					alb.Status = domain.StatusCompleted
					_ = o.repo.SaveAlbum(ctx, alb)
					matchedCount++
				}
				break
			}
		}
	}

	// Pass 2: Any disk folders containing audio that didn't match an indexed release
	// are added as local completed albums so they are tracked and visible in findr
	for _, folderName := range diskFolders {
		if matchedDiskFolders[folderName] {
			continue
		}
		folderPath := filepath.Join(artistDir, folderName)
		if !dirHasAudioFiles(folderPath) {
			continue
		}

		folderTitle, folderYear := parseAlbumFolder(folderName)
		localAlbumID := fmt.Sprintf("local-%x", md5.Sum([]byte(artist.ID+":"+folderTitle)))[:16]

		now := time.Now()
		localAlbum := &domain.Album{
			ID:          localAlbumID,
			ArtistID:    artist.ID,
			ArtistName:  artist.Name,
			Title:       folderTitle,
			ReleaseType: domain.ReleaseTypeAlbum,
			Year:        folderYear,
			Source:      "local",
			SourceID:    localAlbumID,
			Monitored:   false,
			Status:      domain.StatusCompleted,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := o.repo.SaveAlbum(ctx, localAlbum); err == nil {
			matchedCount++
		}
	}

	return matchedCount, nil
}

func parseAlbumFolder(folderName string) (string, int) {
	matches := yearSuffixRegex.FindStringSubmatch(folderName)
	if len(matches) == 2 {
		year, err := strconv.Atoi(matches[1])
		if err == nil {
			title := strings.TrimSpace(folderName[:len(folderName)-len(matches[0])])
			return title, year
		}
	}
	return strings.TrimSpace(folderName), 0
}

func matchAlbum(folderTitle string, folderYear int, albTitle string, albYear int) bool {
	normFolder := normalizeKey(folderTitle)
	normAlb := normalizeKey(albTitle)

	if normFolder == normAlb {
		return true
	}

	// If year matches or is within 1 year (reissue / remaster offset) and one title contains the other
	if len(normFolder) >= 4 && len(normAlb) >= 4 {
		if strings.Contains(normFolder, normAlb) || strings.Contains(normAlb, normFolder) {
			if folderYear > 0 && albYear > 0 {
				diff := folderYear - albYear
				if diff < 0 {
					diff = -diff
				}
				if diff <= 1 {
					return true
				}
			} else {
				return true
			}
		}
	}

	return false
}

func normalizeKey(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func dirHasAudioFiles(dirPath string) bool {
	hasAudio := false
	_ = filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".flac", ".mp3", ".m4a", ".wav", ".ogg", ".aac", ".aiff", ".alac":
				hasAudio = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return hasAudio
}

func isIgnoredDir(name string) bool {
	switch strings.ToLower(name) {
	case "lost+found", "$recycle.bin", "system volume information", "@eadir", ".ds_store":
		return true
	default:
		return false
	}
}
