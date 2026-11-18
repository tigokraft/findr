package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/indexer/deezer"
	qobuzidx "github.com/findr-app/findr/internal/indexer/qobuz"
)

// Catalogs findr can browse regardless of which one is the default indexer.
var knownCatalogs = map[string]bool{"deezer": true, "qobuz": true}

// activeIndexer returns the configured default indexer.
func (o *Orchestrator) activeIndexer() indexer.Indexer {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.indexer
}

// indexerFor returns the indexer that owns IDs from source ("deezer", "qobuz").
// An empty or unknown source, or a non-catalog default indexer (tests), gives
// the default indexer.
func (o *Orchestrator) indexerFor(source string) indexer.Indexer {
	o.mu.RLock()
	active := o.indexer
	appID := ""
	if o.cfg != nil {
		appID = o.cfg.Indexer.Qobuz.AppID
	}
	o.mu.RUnlock()

	source = strings.ToLower(strings.TrimSpace(source))
	if active == nil || source == "" || source == active.Name() || !knownCatalogs[active.Name()] {
		return active
	}
	switch source {
	case "deezer":
		return deezer.NewClient()
	case "qobuz":
		return qobuzidx.NewClient(appID)
	}
	return active
}

// libraryID is the ID a release or artist from source gets in the database.
// Records from the default indexer keep the catalog ID (as before); records
// from another catalog are prefixed so numeric IDs cannot collide.
func (o *Orchestrator) libraryID(source, sourceID string) string {
	active := o.activeIndexer()
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" || active == nil || source == active.Name() || !knownCatalogs[active.Name()] {
		return sourceID
	}
	return source + "-" + sourceID
}

func idCandidates(source, sourceID string) []string {
	if source == "" {
		return []string{sourceID}
	}
	return []string{sourceID, strings.ToLower(source) + "-" + sourceID}
}

func sameSource(recordSource, source string) bool {
	return source == "" || recordSource == "" || strings.EqualFold(recordSource, source)
}

// lookupAlbum finds a library release by catalog and catalog ID, whichever ID
// scheme it was stored under.
func (o *Orchestrator) lookupAlbum(ctx context.Context, source, sourceID string) (*domain.Album, error) {
	for _, id := range idCandidates(source, sourceID) {
		a, err := o.repo.GetAlbum(ctx, id)
		if err != nil {
			return nil, err
		}
		if a != nil && sameSource(a.Source, source) {
			return a, nil
		}
	}
	return nil, nil
}

// lookupArtist finds a library artist by catalog and catalog ID.
func (o *Orchestrator) lookupArtist(ctx context.Context, source, sourceID string) (*domain.Artist, error) {
	for _, id := range idCandidates(source, sourceID) {
		a, err := o.repo.GetArtist(ctx, id)
		if err != nil {
			return nil, err
		}
		if a != nil && sameSource(a.Source, source) {
			return a, nil
		}
	}
	return nil, nil
}

// CatalogAlbum is a catalog release with its tracklist and library state.
type CatalogAlbum struct {
	AlbumSearchResult
	LibraryID string `json:"library_id,omitempty"`
}

// GetCatalogAlbum fetches a release with its tracklist from a catalog and
// annotates it with what the library already knows (status, picked tracks).
func (o *Orchestrator) GetCatalogAlbum(ctx context.Context, source, sourceID string) (*CatalogAlbum, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("album id is required")
	}
	idx := o.indexerFor(source)
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
	normalizeTracks(detailed)
	res := &CatalogAlbum{AlbumSearchResult: AlbumSearchResult{Album: *detailed}}
	if existing, err := o.lookupAlbum(ctx, detailed.Source, detailed.SourceID); err == nil && existing != nil {
		res.InLibrary = true
		res.LibraryID = existing.ID
		res.Status = existing.Status
		res.Monitored = existing.Monitored
		res.WantedTrackIDs = existing.WantedTrackIDs
	}
	return res, nil
}

// TrackSearchResult is a catalog track hit annotated with its catalog.
type TrackSearchResult struct {
	indexer.TrackHit
	Source string `json:"source"`
}

// SearchTracks searches a catalog for individual tracks by free text.
func (o *Orchestrator) SearchTracks(ctx context.Context, source, query string) ([]TrackSearchResult, error) {
	idx := o.indexerFor(source)
	searcher, ok := idx.(indexer.TrackSearcher)
	if !ok {
		return nil, fmt.Errorf("the %s indexer does not support track search", idx.Name())
	}
	hits, err := searcher.SearchTrack(ctx, "", query)
	if err != nil {
		return nil, fmt.Errorf("search tracks '%s': %w", query, err)
	}
	out := make([]TrackSearchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, TrackSearchResult{TrackHit: h, Source: idx.Name()})
	}
	return out, nil
}

// AddReleaseRequest describes a release (or some of its tracks) to track.
type AddReleaseRequest struct {
	Source   string   `json:"source"`
	SourceID string   `json:"source_id"`
	TrackIDs []string `json:"track_ids,omitempty"` // empty: the whole release
	Download bool     `json:"download"`
}

// mergeWanted combines the tracks already picked for a release with new picks.
// An empty list means "whole release", which absorbs any subset.
func mergeWanted(existing *domain.Album, picked []string) (wanted []string, changed bool) {
	if len(picked) == 0 {
		return nil, existing.IsPartial()
	}
	if !existing.IsPartial() {
		if existing.Status == domain.StatusCompleted {
			return nil, false // the whole release is already on disk
		}
		return dedupe(picked), true
	}
	seen := make(map[string]bool, len(existing.WantedTrackIDs))
	wanted = append(wanted, existing.WantedTrackIDs...)
	for _, id := range existing.WantedTrackIDs {
		seen[id] = true
	}
	for _, id := range picked {
		if id != "" && !seen[id] {
			seen[id] = true
			wanted = append(wanted, id)
			changed = true
		}
	}
	// Every track picked is the same as the whole release.
	if len(existing.Tracks) > 0 && len(wanted) >= len(existing.Tracks) {
		return nil, true
	}
	return wanted, changed
}

func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
