package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/links"
	"github.com/findr-app/findr/internal/verifier"
)

// LinkResolution is what a pasted link turned out to be in findr's catalogs.
type LinkResolution struct {
	Platform     string `json:"platform"`
	PlatformName string `json:"platform_name"`
	Kind         string `json:"kind"` // artist, album, track, playlist
	URL          string `json:"url"`

	Artist *domain.Artist `json:"artist,omitempty"`
	Album  *CatalogAlbum  `json:"album,omitempty"`
	// TrackID is the picked track inside Album for a track link.
	TrackID string `json:"track_id,omitempty"`

	// Matched is true when findr had to search by name (the link's platform is
	// not a catalog findr reads), so the user should check the result.
	Matched bool   `json:"matched"`
	Query   string `json:"query,omitempty"`
}

// linkFetcher is replaced in tests.
var linkFetcher = links.NewFetcher()

// ResolveLink turns a streaming-platform link into a catalog artist, release or
// track. Deezer and Qobuz links resolve directly; other platforms are matched
// by name in the default catalog.
func (o *Orchestrator) ResolveLink(ctx context.Context, raw string) (*LinkResolution, error) {
	ref, err := links.Parse(raw)
	if err != nil {
		return nil, err
	}
	if ref.Kind == links.KindShort {
		if ref, err = linkFetcher.Expand(ctx, ref); err != nil {
			return nil, err
		}
	}
	res := &LinkResolution{Platform: ref.Platform, PlatformName: links.PlatformNames[ref.Platform], Kind: string(ref.Kind), URL: ref.URL}

	if ref.Kind == links.KindPlaylist {
		if ref.Platform == links.Spotify {
			return res, nil // the UI hands these to the playlist importer
		}
		return nil, fmt.Errorf("only Spotify playlists can be imported; this is a %s playlist", res.PlatformName)
	}

	// Catalogs findr reads natively.
	if ref.Platform == links.Deezer || ref.Platform == links.Qobuz {
		nativeErr := o.resolveNative(ctx, ref.Platform, ref.Kind, ref.ID, res)
		if nativeErr == nil {
			return res, nil
		}
		// The Qobuz API refuses some app IDs; a store link's slug still names
		// the item, so fall back to matching it by name.
		if ref.Platform != links.Qobuz || ref.Slug == "" {
			return nil, nativeErr
		}
		fmt.Printf("[findr Links] %v; matching the Qobuz link by name instead\n", nativeErr)
	}

	meta, err := linkFetcher.Lookup(ctx, ref)
	if err != nil {
		return nil, err
	}

	res.Matched = true
	switch ref.Kind {
	case links.KindArtist:
		name := meta.Artist
		if name == "" {
			name = meta.Title
		}
		res.Query = name
		artist, err := o.bestArtist(ctx, name)
		if err != nil {
			return nil, err
		}
		res.Artist = artist
	case links.KindAlbum:
		res.Query = strings.TrimSpace(meta.Artist + " " + meta.Title)
		album, err := o.bestAlbum(ctx, meta.Artist, meta.Title)
		if err != nil {
			return nil, err
		}
		res.Album = album
	case links.KindTrack:
		res.Query = strings.TrimSpace(meta.Artist + " " + meta.Title)
		album, trackID, err := o.bestTrack(ctx, meta.Artist, meta.Title)
		if err != nil {
			return nil, err
		}
		res.Album, res.TrackID = album, trackID
	default:
		return nil, fmt.Errorf("unsupported link type %q", ref.Kind)
	}
	return res, nil
}

func (o *Orchestrator) resolveNative(ctx context.Context, source string, kind links.Kind, id string, res *LinkResolution) error {
	idx := o.indexerFor(source)
	if idx == nil {
		return fmt.Errorf("no indexer for %s", source)
	}
	switch kind {
	case links.KindArtist:
		a, err := idx.GetArtist(ctx, id)
		if err != nil {
			return fmt.Errorf("fetch artist %s from %s: %w", id, idx.Name(), err)
		}
		if a == nil || a.Name == "" {
			return fmt.Errorf("artist %s not found on %s", id, idx.Name())
		}
		if a.Source == "" {
			a.Source = idx.Name()
		}
		if existing, err := o.lookupArtist(ctx, a.Source, a.SourceID); err == nil && existing != nil {
			a.Monitored = true // in library
			a.ID = existing.ID
		}
		res.Artist = a
	case links.KindAlbum:
		album, err := o.GetCatalogAlbum(ctx, idx.Name(), id)
		if err != nil {
			return err
		}
		res.Album = album
	case links.KindTrack:
		getter, ok := idx.(indexer.TrackGetter)
		if !ok {
			return fmt.Errorf("the %s indexer cannot look up tracks", idx.Name())
		}
		hit, err := getter.GetTrack(ctx, id)
		if err != nil {
			return fmt.Errorf("fetch track %s from %s: %w", id, idx.Name(), err)
		}
		if hit == nil {
			return fmt.Errorf("track %s not found on %s", id, idx.Name())
		}
		album, err := o.GetCatalogAlbum(ctx, idx.Name(), hit.AlbumID)
		if err != nil {
			return err
		}
		res.Album, res.TrackID = album, trackIDInAlbum(album, hit)
	default:
		return fmt.Errorf("unsupported link type %q", kind)
	}
	return nil
}

// trackIDInAlbum maps a track hit to the ID used in the album's tracklist.
func trackIDInAlbum(album *CatalogAlbum, hit *indexer.TrackHit) string {
	for _, t := range album.Tracks {
		if t.ID == hit.TrackID || t.SourceID == hit.TrackID {
			return t.ID
		}
	}
	if hit.ISRC != "" {
		for _, t := range album.Tracks {
			if strings.EqualFold(t.ISRC, hit.ISRC) {
				return t.ID
			}
		}
	}
	best, bestScore := "", 0.0
	for _, t := range album.Tracks {
		if s := verifier.StringSimilarity(t.Title, hit.Title); s > bestScore {
			best, bestScore = t.ID, s
		}
	}
	if bestScore >= 0.8 {
		return best
	}
	return hit.TrackID
}

func (o *Orchestrator) bestArtist(ctx context.Context, name string) (*domain.Artist, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("the link did not say which artist it is")
	}
	artists, err := o.SearchArtists(ctx, "", name)
	if err != nil {
		return nil, err
	}
	if len(artists) == 0 {
		return nil, fmt.Errorf("no artist called %q in the catalog", name)
	}
	for i := range artists {
		if strings.EqualFold(strings.TrimSpace(artists[i].Name), strings.TrimSpace(name)) {
			return &artists[i], nil
		}
	}
	best, bestScore := 0, 0.0
	for i := range artists {
		if s := verifier.StringSimilarity(strings.ToLower(artists[i].Name), strings.ToLower(name)); s > bestScore {
			best, bestScore = i, s
		}
	}
	return &artists[best], nil
}

// matchScore rates a catalog item against what the link said. Without an
// artist, the title may hold artist and title words mixed (a URL slug), so it
// is compared against both orders of "artist title".
func matchScore(itemTitle, itemArtist, title, artist string) float64 {
	low := strings.ToLower
	if artist != "" {
		return 0.6*verifier.StringSimilarity(low(itemTitle), low(title)) + 0.4*verifier.StringSimilarity(low(itemArtist), low(artist))
	}
	s := verifier.StringSimilarity(low(itemTitle), low(title))
	if v := verifier.StringSimilarity(low(itemTitle+" "+itemArtist), low(title)); v > s {
		s = v
	}
	if v := verifier.StringSimilarity(low(itemArtist+" "+itemTitle), low(title)); v > s {
		s = v
	}
	return s
}

func (o *Orchestrator) bestAlbum(ctx context.Context, artist, title string) (*CatalogAlbum, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("the link did not say which release it is")
	}
	idx := o.activeIndexer()
	searcher, ok := idx.(indexer.AlbumSearcher)
	if !ok {
		return nil, fmt.Errorf("the %s indexer does not support album search", idx.Name())
	}
	albums, err := searcher.SearchAlbum(ctx, strings.TrimSpace(artist+" "+title))
	if err != nil {
		return nil, err
	}
	if len(albums) == 0 && artist != "" {
		albums, _ = searcher.SearchAlbum(ctx, title)
	}
	if len(albums) == 0 {
		return nil, fmt.Errorf("no release called %q in the catalog", title)
	}
	best, bestScore := 0, -1.0
	for i, a := range albums {
		if s := matchScore(a.Title, a.ArtistName, title, artist); s > bestScore {
			best, bestScore = i, s
		}
	}
	src := albums[best].Source
	if src == "" {
		src = idx.Name()
	}
	return o.GetCatalogAlbum(ctx, src, albums[best].SourceID)
}

func (o *Orchestrator) bestTrack(ctx context.Context, artist, title string) (*CatalogAlbum, string, error) {
	if strings.TrimSpace(title) == "" {
		return nil, "", fmt.Errorf("the link did not say which track it is")
	}
	idx := o.activeIndexer()
	searcher, ok := idx.(indexer.TrackSearcher)
	if !ok {
		return nil, "", fmt.Errorf("the %s indexer does not support track search", idx.Name())
	}
	hits, err := searcher.SearchTrack(ctx, artist, title)
	if err != nil {
		return nil, "", err
	}
	if len(hits) == 0 {
		return nil, "", fmt.Errorf("no track called %q in the catalog", title)
	}
	best, bestScore := 0, -1.0
	for i, h := range hits {
		if s := matchScore(h.Title, h.ArtistName, title, artist); s > bestScore {
			best, bestScore = i, s
		}
	}
	hit := hits[best]
	album, err := o.GetCatalogAlbum(ctx, idx.Name(), hit.AlbumID)
	if err != nil {
		return nil, "", err
	}
	return album, trackIDInAlbum(album, &hit), nil
}
