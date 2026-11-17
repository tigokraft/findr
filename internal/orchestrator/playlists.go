package orchestrator

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/spotify"
	"github.com/findr-app/findr/internal/verifier"
)

const (
	previewTTL         = 30 * time.Minute
	minTrackMatchScore = 0.72
	resolveWorkers     = 4
	// Deezer allows ~50 requests per 5 s; one resolve is 1-2 requests.
	resolvePace = 200 * time.Millisecond
)

// PlaylistPreviewAlbum is one release a playlist's tracks resolved to.
type PlaylistPreviewAlbum struct {
	AlbumID        string                `json:"album_id"`
	Title          string                `json:"title"`
	ArtistName     string                `json:"artist_name"`
	CoverURL       string                `json:"cover_url,omitempty"`
	PlaylistTracks int                   `json:"playlist_tracks"`
	InLibrary      bool                  `json:"in_library"`
	Status         domain.DownloadStatus `json:"status,omitempty"`
	Monitored      bool                  `json:"monitored"`
	Excluded       bool                  `json:"excluded,omitempty"`
}

// PlaylistPreview is a fetched and resolved playlist that has not been saved yet.
type PlaylistPreview struct {
	Playlist   domain.Playlist        `json:"playlist"`
	Albums     []PlaylistPreviewAlbum `json:"albums"`
	Resolved   int                    `json:"resolved"`
	Unresolved int                    `json:"unresolved"`
	Via        string                 `json:"via"`
	Partial    bool                   `json:"partial"`
	Indexer    string                 `json:"indexer"`
}

type cachedPreview struct {
	preview *PlaylistPreview
	at      time.Time
}

// PlaylistSummary is a saved playlist with library progress counters.
type PlaylistSummary struct {
	domain.Playlist
	TrackCount      int `json:"track_count"`
	ResolvedCount   int `json:"resolved_count"`
	AlbumCount      int `json:"album_count"`
	DownloadedCount int `json:"downloaded_count"` // tracks whose release is on disk
}

// M3UResult reports a playlist file write.
type M3UResult struct {
	Path    string `json:"path"`
	Found   int    `json:"found"`
	Missing int    `json:"missing"`
}

func (o *Orchestrator) spotifyAPI() *spotify.Client {
	o.mu.RLock()
	id, secret := "", ""
	if o.cfg != nil {
		id, secret = o.cfg.Indexer.Spotify.ClientID, o.cfg.Indexer.Spotify.ClientSecret
	}
	o.mu.RUnlock()

	o.playlistMu.Lock()
	defer o.playlistMu.Unlock()
	key := id + "\x00" + secret
	if o.spotifyClient == nil || o.spotifyKey != key {
		o.spotifyClient = spotify.NewClient(id, secret)
		o.spotifyKey = key
	}
	return o.spotifyClient
}

func playlistDBID(spotifyID string) string { return "spotify-" + spotifyID }

// PreviewPlaylist fetches a Spotify playlist and resolves every track to a
// release on the active indexer, without saving anything.
func (o *Orchestrator) PreviewPlaylist(ctx context.Context, input string) (*PlaylistPreview, error) {
	spID, err := spotify.ParsePlaylistID(input)
	if err != nil {
		return nil, err
	}
	sp, err := o.spotifyAPI().GetPlaylist(ctx, spID)
	if err != nil {
		return nil, err
	}

	o.mu.RLock()
	idx := o.indexer
	o.mu.RUnlock()

	tracks, err := resolvePlaylistTracks(ctx, idx, sp.Tracks)
	if err != nil {
		return nil, err
	}

	pl := domain.Playlist{
		ID:        playlistDBID(spID),
		Source:    "spotify",
		SourceID:  spID,
		URL:       sp.URL,
		Name:      sp.Name,
		Owner:     sp.Owner,
		ImageURL:  sp.ImageURL,
		Monitored: true,
		Tracks:    tracks,
	}
	if existing, _ := o.repo.GetPlaylist(ctx, pl.ID); existing != nil {
		pl.ExcludedAlbumIDs = existing.ExcludedAlbumIDs
		pl.Monitored = existing.Monitored
	}

	preview := &PlaylistPreview{
		Playlist: pl,
		Via:      sp.Via,
		Partial:  sp.Partial,
		Indexer:  idx.Name(),
	}
	preview.Albums = o.groupPlaylistAlbums(ctx, &pl)
	for _, t := range tracks {
		if t.AlbumID != "" {
			preview.Resolved++
		} else {
			preview.Unresolved++
		}
	}

	o.playlistMu.Lock()
	if o.previewCache == nil {
		o.previewCache = make(map[string]*cachedPreview)
	}
	for k, v := range o.previewCache {
		if time.Since(v.at) > previewTTL {
			delete(o.previewCache, k)
		}
	}
	o.previewCache[spID] = &cachedPreview{preview: preview, at: time.Now()}
	o.playlistMu.Unlock()

	return preview, nil
}

func (o *Orchestrator) cachedOrFreshPreview(ctx context.Context, input string) (*PlaylistPreview, error) {
	spID, err := spotify.ParsePlaylistID(input)
	if err != nil {
		return nil, err
	}
	o.playlistMu.Lock()
	c := o.previewCache[spID]
	o.playlistMu.Unlock()
	if c != nil && time.Since(c.at) < previewTTL {
		return c.preview, nil
	}
	return o.PreviewPlaylist(ctx, input)
}

// groupPlaylistAlbums collapses resolved tracks into their releases, most-used first.
func (o *Orchestrator) groupPlaylistAlbums(ctx context.Context, pl *domain.Playlist) []PlaylistPreviewAlbum {
	excluded := make(map[string]bool, len(pl.ExcludedAlbumIDs))
	for _, id := range pl.ExcludedAlbumIDs {
		excluded[id] = true
	}
	byID := map[string]*PlaylistPreviewAlbum{}
	var order []string
	for _, t := range pl.Tracks {
		if t.AlbumID == "" {
			continue
		}
		a := byID[t.AlbumID]
		if a == nil {
			a = &PlaylistPreviewAlbum{
				AlbumID:    t.AlbumID,
				Title:      t.MatchedAlbum,
				ArtistName: t.MatchedArtist,
				CoverURL:   t.MatchedCoverURL,
				Excluded:   excluded[t.AlbumID],
			}
			if existing, err := o.repo.GetAlbum(ctx, t.AlbumID); err == nil && existing != nil {
				a.InLibrary = true
				a.Status = existing.Status
				a.Monitored = existing.Monitored
				if existing.ArtistName != "" {
					a.ArtistName = existing.ArtistName
				}
			}
			byID[t.AlbumID] = a
			order = append(order, t.AlbumID)
		}
		a.PlaylistTracks++
	}
	out := make([]PlaylistPreviewAlbum, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PlaylistTracks > out[j].PlaylistTracks })
	return out
}

// PlaylistImportResult reports what an import or sync did.
type PlaylistImportResult struct {
	Playlist      domain.Playlist `json:"playlist"`
	AlbumsToTrack int             `json:"albums_to_track"`
	Message       string          `json:"message"`
}

// ImportPlaylist saves a playlist and tracks the releases its tracks resolved to.
// albumIDs limits which releases are tracked (nil means all); the rest are
// remembered as excluded so later syncs skip them. Tracking runs in the
// background because each release needs an indexer lookup.
func (o *Orchestrator) ImportPlaylist(ctx context.Context, input string, albumIDs []string, download, monitor bool) (*PlaylistImportResult, error) {
	preview, err := o.cachedOrFreshPreview(ctx, input)
	if err != nil {
		return nil, err
	}
	pl := preview.Playlist
	pl.Monitored = monitor
	pl.LastSyncedAt = time.Now()
	if existing, _ := o.repo.GetPlaylist(ctx, pl.ID); existing != nil {
		pl.CreatedAt = existing.CreatedAt
		pl.M3UPath = existing.M3UPath
	}

	var selected []string
	if albumIDs == nil {
		excluded := toSet(pl.ExcludedAlbumIDs)
		for _, a := range preview.Albums {
			if !excluded[a.AlbumID] {
				selected = append(selected, a.AlbumID)
			}
		}
	} else {
		want := toSet(albumIDs)
		pl.ExcludedAlbumIDs = nil
		for _, a := range preview.Albums {
			if want[a.AlbumID] {
				selected = append(selected, a.AlbumID)
			} else {
				pl.ExcludedAlbumIDs = append(pl.ExcludedAlbumIDs, a.AlbumID)
			}
		}
	}

	if err := o.repo.SavePlaylist(ctx, &pl); err != nil {
		return nil, fmt.Errorf("save playlist: %w", err)
	}

	// A sync (albumIDs == nil) only adds releases that are new to the library so
	// it never re-monitors something the user switched off.
	go o.trackPlaylistAlbums(pl.ID, selected, download, albumIDs == nil)

	verb := "Tracking"
	if download {
		verb = "Tracking and downloading"
	}
	return &PlaylistImportResult{
		Playlist:      pl,
		AlbumsToTrack: len(selected),
		Message:       fmt.Sprintf("%s %d releases from %s", verb, len(selected), pl.Name),
	}, nil
}

func (o *Orchestrator) trackPlaylistAlbums(playlistID string, albumIDs []string, download, onlyNew bool) {
	ctx := context.Background()
	tracked, failed := 0, 0
	for _, id := range albumIDs {
		if onlyNew {
			if existing, err := o.repo.GetAlbum(ctx, id); err == nil && existing != nil {
				continue
			}
		}
		if _, err := o.AddAlbum(ctx, id, download); err != nil {
			failed++
			fmt.Printf("[findr Playlist] Could not track album %s: %v\n", id, err)
			continue
		}
		tracked++
	}
	fmt.Printf("[findr Playlist] %s: tracked %d releases (%d failed)\n", playlistID, tracked, failed)
	if pl, err := o.repo.GetPlaylist(ctx, playlistID); err == nil && pl != nil {
		o.m3uMu.Lock()
		_, _ = o.writePlaylistM3U(ctx, pl)
		o.m3uMu.Unlock()
	}
}

// SyncPlaylist re-reads a saved playlist from Spotify, resolves new tracks and
// tracks their releases (skipping excluded ones).
func (o *Orchestrator) SyncPlaylist(ctx context.Context, id string, download bool) (*PlaylistImportResult, error) {
	pl, err := o.repo.GetPlaylist(ctx, id)
	if err != nil || pl == nil {
		return nil, fmt.Errorf("playlist %s not found", id)
	}
	input := pl.SourceID
	o.playlistMu.Lock()
	delete(o.previewCache, pl.SourceID)
	o.playlistMu.Unlock()
	return o.ImportPlaylist(ctx, input, nil, download, pl.Monitored)
}

// SyncMonitoredPlaylists refreshes every monitored playlist (scheduler hook).
func (o *Orchestrator) SyncMonitoredPlaylists(ctx context.Context) {
	pls, err := o.repo.ListPlaylists(ctx)
	if err != nil {
		return
	}
	for _, pl := range pls {
		if !pl.Monitored {
			continue
		}
		if _, err := o.SyncPlaylist(ctx, pl.ID, false); err != nil {
			fmt.Printf("[Scheduler] Playlist '%s' sync failed: %v\n", pl.Name, err)
		}
	}
}

// ListPlaylistSummaries returns saved playlists with library progress.
func (o *Orchestrator) ListPlaylistSummaries(ctx context.Context) ([]PlaylistSummary, error) {
	pls, err := o.repo.ListPlaylists(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PlaylistSummary, 0, len(pls))
	for _, pl := range pls {
		out = append(out, o.summarize(ctx, pl))
	}
	return out, nil
}

// GetPlaylistDetail returns a saved playlist with per-release state.
func (o *Orchestrator) GetPlaylistDetail(ctx context.Context, id string) (*PlaylistSummary, []PlaylistPreviewAlbum, error) {
	pl, err := o.repo.GetPlaylist(ctx, id)
	if err != nil || pl == nil {
		return nil, nil, fmt.Errorf("playlist %s not found", id)
	}
	s := o.summarize(ctx, *pl)
	return &s, o.groupPlaylistAlbums(ctx, pl), nil
}

func (o *Orchestrator) summarize(ctx context.Context, pl domain.Playlist) PlaylistSummary {
	s := PlaylistSummary{Playlist: pl, TrackCount: len(pl.Tracks)}
	status := map[string]domain.DownloadStatus{}
	for _, t := range pl.Tracks {
		if t.AlbumID == "" {
			continue
		}
		s.ResolvedCount++
		st, seen := status[t.AlbumID]
		if !seen {
			if a, err := o.repo.GetAlbum(ctx, t.AlbumID); err == nil && a != nil {
				st = a.Status
			}
			status[t.AlbumID] = st
		}
		if st == domain.StatusCompleted {
			s.DownloadedCount++
		}
	}
	excluded := toSet(pl.ExcludedAlbumIDs)
	for id := range status {
		if !excluded[id] {
			s.AlbumCount++
		}
	}
	return s
}

// EnqueuePlaylistMissing queues every tracked, not-yet-downloaded release of a playlist.
func (o *Orchestrator) EnqueuePlaylistMissing(ctx context.Context, id string) (int, error) {
	pl, err := o.repo.GetPlaylist(ctx, id)
	if err != nil || pl == nil {
		return 0, fmt.Errorf("playlist %s not found", id)
	}
	excluded := toSet(pl.ExcludedAlbumIDs)
	seen := map[string]bool{}
	var ids []string
	for _, t := range pl.Tracks {
		if t.AlbumID == "" || excluded[t.AlbumID] || seen[t.AlbumID] {
			continue
		}
		seen[t.AlbumID] = true
		a, err := o.repo.GetAlbum(ctx, t.AlbumID)
		if err != nil || a == nil || a.Status == domain.StatusCompleted {
			continue
		}
		ids = append(ids, a.ID)
	}
	return o.EnqueueBatchDownloads(ids)
}

// DeletePlaylist removes a saved playlist. Tracked releases stay in the library.
func (o *Orchestrator) DeletePlaylist(ctx context.Context, id string, removeM3U bool) error {
	pl, err := o.repo.GetPlaylist(ctx, id)
	if err != nil || pl == nil {
		return fmt.Errorf("playlist %s not found", id)
	}
	if removeM3U && pl.M3UPath != "" {
		if musicPath := o.MusicPath(); musicPath != "" && within(musicPath, pl.M3UPath) {
			_ = os.Remove(pl.M3UPath)
		}
	}
	return o.repo.DeletePlaylist(ctx, id)
}

// WritePlaylistM3U (re)writes the playlist's .m3u8 file in <music>/Playlists.
func (o *Orchestrator) WritePlaylistM3U(ctx context.Context, id string) (*M3UResult, error) {
	pl, err := o.repo.GetPlaylist(ctx, id)
	if err != nil || pl == nil {
		return nil, fmt.Errorf("playlist %s not found", id)
	}
	o.m3uMu.Lock()
	defer o.m3uMu.Unlock()
	return o.writePlaylistM3U(ctx, pl)
}

func (o *Orchestrator) refreshPlaylistsForAlbum(ctx context.Context, albumID string) {
	pls, err := o.repo.ListPlaylists(ctx)
	if err != nil || len(pls) == 0 {
		return
	}
	o.m3uMu.Lock()
	defer o.m3uMu.Unlock()
	for i := range pls {
		for _, t := range pls[i].Tracks {
			if t.AlbumID == albumID {
				if _, err := o.writePlaylistM3U(ctx, &pls[i]); err != nil {
					fmt.Printf("[findr Playlist] M3U refresh for '%s' failed: %v\n", pls[i].Name, err)
				}
				break
			}
		}
	}
}

type albumFiles struct {
	album *domain.Album
	files []string
}

// writePlaylistM3U must be called with m3uMu held.
func (o *Orchestrator) writePlaylistM3U(ctx context.Context, pl *domain.Playlist) (*M3UResult, error) {
	musicPath := o.MusicPath()
	if strings.TrimSpace(musicPath) == "" {
		return nil, fmt.Errorf("music_path is not configured")
	}
	dir := filepath.Join(musicPath, "Playlists")
	name := safeFileName(pl.Name)
	if name == "" {
		name = pl.SourceID
	}
	path := filepath.Join(dir, name+".m3u8")
	res := &M3UResult{Path: path}

	cache := map[string]*albumFiles{}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#PLAYLIST:" + strings.ReplaceAll(pl.Name, "\n", " ") + "\n")
	for _, t := range pl.Tracks {
		if t.AlbumID == "" {
			res.Missing++
			continue
		}
		af, ok := cache[t.AlbumID]
		if !ok {
			af = &albumFiles{}
			if a, err := o.repo.GetAlbum(ctx, t.AlbumID); err == nil && a != nil {
				af.album = a
				if d := o.locateAlbumDir(a); d != "" {
					af.files = audioFilesUnder(d)
				}
			}
			cache[t.AlbumID] = af
		}
		file := o.findTrackFile(af, t)
		if file == "" {
			res.Missing++
			continue
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			rel = file
		}
		artist := strings.Join(t.Artists, ", ")
		fmt.Fprintf(&b, "#EXTINF:%d,%s - %s\n%s\n", t.DurationSec, artist, t.Title, filepath.ToSlash(rel))
		res.Found++
	}

	if res.Found == 0 {
		return res, nil // nothing on disk yet; do not create an empty playlist
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	// Remove the previous file if the playlist was renamed on Spotify.
	if pl.M3UPath != "" && pl.M3UPath != path && within(musicPath, pl.M3UPath) {
		_ = os.Remove(pl.M3UPath)
	}
	if pl.M3UPath != path {
		pl.M3UPath = path
		_ = o.repo.SavePlaylist(ctx, pl)
	}
	return res, nil
}

var leadingTrackNo = regexp.MustCompile(`^\s*(\d{1,2}[-.])?\d{1,3}\s*[-._ ]+\s*`)

// findTrackFile picks the audio file for a playlist track inside its release folder.
func (o *Orchestrator) findTrackFile(af *albumFiles, t domain.PlaylistTrack) string {
	if af == nil || af.album == nil || len(af.files) == 0 {
		return ""
	}
	want := t.MatchedTitle
	if want == "" {
		want = t.Title
	}

	// 1. Find the release track, then the file the importer would have named for it.
	var best *domain.Track
	bestScore := 0.0
	for i := range af.album.Tracks {
		at := &af.album.Tracks[i]
		if t.ISRC != "" && strings.EqualFold(at.ISRC, t.ISRC) {
			best, bestScore = at, 1
			break
		}
		if s := verifier.StringSimilarity(cleanTitle(at.Title), cleanTitle(want)); s > bestScore {
			best, bestScore = at, s
		}
	}
	o.mu.RLock()
	imp := o.importer
	o.mu.RUnlock()
	if best != nil && bestScore >= 0.8 && imp != nil {
		stem := strings.TrimSuffix(imp.BuildTrackFilename(af.album, *best, ".x"), ".x")
		for _, f := range af.files {
			base := filepath.Base(f)
			if strings.EqualFold(strings.TrimSuffix(base, filepath.Ext(base)), stem) {
				return f
			}
		}
		want = best.Title
	}

	// 2. Fall back to comparing file names with the title.
	bestFile, bestFileScore := "", 0.0
	for _, f := range af.files {
		base := filepath.Base(f)
		stem := leadingTrackNo.ReplaceAllString(strings.TrimSuffix(base, filepath.Ext(base)), "")
		if s := verifier.StringSimilarity(cleanTitle(stem), cleanTitle(want)); s > bestFileScore {
			bestFile, bestFileScore = f, s
		}
	}
	if bestFileScore >= 0.85 {
		return bestFile
	}
	return ""
}

func audioFilesUnder(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".flac", ".mp3", ".m4a", ".aac", ".wav", ".ogg", ".opus", ".aiff", ".alac", ".wv":
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ---- track resolution ----

var (
	featRe    = regexp.MustCompile(`(?i)\s*[\(\[](feat\.?|ft\.?|featuring|with)\s[^\)\]]*[\)\]]`)
	dashTagRe = regexp.MustCompile(`(?i)\s+-\s+.*\b(remaster(ed)?|version|edit|mono|stereo|live|mix|remix|single|radio|acoustic|from|bonus|deluxe)\b.*$`)
)

// cleanTitle drops featured-artist credits and Spotify's " - Remastered 2011"
// style suffixes so titles from different catalogs compare cleanly.
func cleanTitle(s string) string {
	s = featRe.ReplaceAllString(s, "")
	s = dashTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func resolvePlaylistTracks(ctx context.Context, idx indexer.Indexer, tracks []spotify.Track) ([]domain.PlaylistTrack, error) {
	searcher, canSearch := idx.(indexer.TrackSearcher)
	isrcer, canISRC := idx.(indexer.ISRCResolver)
	if !canSearch && !canISRC {
		return nil, fmt.Errorf("the %s indexer cannot resolve playlist tracks", idx.Name())
	}

	out := make([]domain.PlaylistTrack, len(tracks))
	pace := time.NewTicker(resolvePace)
	defer pace.Stop()

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < resolveWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				select {
				case <-ctx.Done():
					return
				case <-pace.C:
				}
				out[i] = resolveTrack(ctx, searcher, isrcer, tracks[i], i+1)
			}
		}()
	}
	for i := range tracks {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return out, ctx.Err()
}

func resolveTrack(ctx context.Context, searcher indexer.TrackSearcher, isrcer indexer.ISRCResolver, t spotify.Track, pos int) domain.PlaylistTrack {
	pt := domain.PlaylistTrack{
		Position:    pos,
		Title:       t.Title,
		Artists:     t.Artists,
		AlbumTitle:  t.Album,
		DurationSec: t.DurationSec,
		ISRC:        t.ISRC,
		SourceURI:   t.URI,
	}
	apply := func(h indexer.TrackHit, score float64) {
		pt.AlbumID = h.AlbumID
		pt.MatchedTitle = h.Title
		pt.MatchedArtist = h.ArtistName
		pt.MatchedAlbum = h.AlbumTitle
		pt.MatchedCoverURL = h.CoverURL
		pt.MatchScore = score
	}

	if t.ISRC != "" && isrcer != nil {
		if h, err := isrcer.LookupISRC(ctx, t.ISRC); err == nil && h != nil && h.AlbumID != "" {
			apply(*h, 1)
			return pt
		}
	}
	if searcher == nil {
		return pt
	}
	primary := ""
	if len(t.Artists) > 0 {
		primary = t.Artists[0]
	}
	hits, err := searcher.SearchTrack(ctx, primary, cleanTitle(t.Title))
	if err != nil || len(hits) == 0 {
		return pt
	}
	bestIdx, bestScore := -1, 0.0
	for i, h := range hits {
		if s := scoreTrackHit(t, h); s > bestScore {
			bestIdx, bestScore = i, s
		}
	}
	if bestIdx >= 0 && bestScore >= minTrackMatchScore && hits[bestIdx].AlbumID != "" {
		apply(hits[bestIdx], bestScore)
	}
	return pt
}

// scoreTrackHit weighs title (60%), artist (30%) and duration (10%) agreement.
func scoreTrackHit(t spotify.Track, h indexer.TrackHit) float64 {
	if t.ISRC != "" && strings.EqualFold(t.ISRC, h.ISRC) {
		return 1
	}
	title := verifier.StringSimilarity(cleanTitle(h.Title), cleanTitle(t.Title))

	artist := 0.0
	for _, a := range t.Artists {
		s := verifier.StringSimilarity(a, h.ArtistName)
		if strings.Contains(strings.ToLower(h.ArtistName), strings.ToLower(a)) && a != "" {
			s = 1
		}
		if s > artist {
			artist = s
		}
	}

	dur := 0.5 // unknown
	if t.DurationSec > 0 && h.DurationSec > 0 {
		diff := t.DurationSec - h.DurationSec
		if diff < 0 {
			diff = -diff
		}
		switch {
		case diff <= 3:
			dur = 1
		case diff <= 10:
			dur = 0.5
		default:
			dur = 0
		}
	}
	return 0.6*title + 0.3*artist + 0.1*dur
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeFileName(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", "\x00", "")
	s := strings.TrimSpace(r.Replace(name))
	return strings.TrimLeft(s, ".")
}
