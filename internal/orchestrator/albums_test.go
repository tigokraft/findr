package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/spotify"
)

// catalogIndexer extends mockIndexer with artist lookup, releases and track search.
type catalogIndexer struct {
	mockIndexer
	releases []domain.Album
	hits     map[string][]indexer.TrackHit // keyed by lowercased title
}

func (c *catalogIndexer) GetArtist(ctx context.Context, id string) (*domain.Artist, error) {
	return &domain.Artist{ID: id, SourceID: id, Name: "Catalog Artist", ImageURL: "https://img/a", Source: "mock"}, nil
}
func (c *catalogIndexer) GetArtistReleases(ctx context.Context, artistID string, f indexer.ReleaseFilter) ([]domain.Album, error) {
	out := make([]domain.Album, len(c.releases))
	copy(out, c.releases)
	return out, nil
}
func (c *catalogIndexer) SearchTrack(ctx context.Context, artist, title string) ([]indexer.TrackHit, error) {
	return c.hits[strings.ToLower(title)], nil
}

func useIndexer(o *Orchestrator, idx indexer.Indexer) {
	o.mu.Lock()
	o.indexer = idx
	o.mu.Unlock()
}

func TestAddAlbum_TracksReleaseWithUnmonitoredArtist(t *testing.T) {
	orch, repo := setupTestOrchestrator(t, 1, newMockDownloader(0, nil))
	defer orch.StopQueue()
	useIndexer(orch, &catalogIndexer{})
	ctx := context.Background()

	album, err := orch.AddAlbum(ctx, "alb-42", false)
	if err != nil {
		t.Fatal(err)
	}
	if !album.Monitored || album.ArtistID != "art-1" || album.ArtistName != "Catalog Artist" {
		t.Fatalf("unexpected album: %+v", album)
	}
	artist, _ := repo.GetArtist(ctx, "art-1")
	if artist == nil || artist.Monitored || artist.Name != "Catalog Artist" {
		t.Fatalf("artist should exist unmonitored: %+v", artist)
	}

	// Adding again keeps it single and does not error.
	if _, err := orch.AddAlbum(ctx, "alb-42", false); err != nil {
		t.Fatal(err)
	}
	albums, _ := repo.ListAlbumsByArtist(ctx, "art-1")
	if len(albums) != 1 {
		t.Fatalf("expected only the tracked album, got %d", len(albums))
	}
}

func TestAddArtist_PreservesExistingAlbumState(t *testing.T) {
	orch, repo := setupTestOrchestrator(t, 1, newMockDownloader(0, nil))
	defer orch.StopQueue()
	ctx := context.Background()
	useIndexer(orch, &catalogIndexer{releases: []domain.Album{
		{ID: "r1", Title: "Done", Status: domain.StatusPending, Source: "mock", SourceID: "r1"},
		{ID: "r2", Title: "Off", Status: domain.StatusPending, Source: "mock", SourceID: "r2"},
		{ID: "r3", Title: "New", Status: domain.StatusPending, Source: "mock", SourceID: "r3"},
	}})

	_ = repo.SaveArtist(ctx, &domain.Artist{ID: "art-1", Name: "A", Source: "mock", SourceID: "art-1", Monitored: true})
	_ = repo.SaveAlbum(ctx, &domain.Album{ID: "r1", ArtistID: "art-1", ArtistName: "A", Title: "Done", Source: "mock", SourceID: "r1", Monitored: true, Status: domain.StatusCompleted,
		Tracks: []domain.Track{{ID: "t", Title: "x"}}})
	_ = repo.SaveAlbum(ctx, &domain.Album{ID: "r2", ArtistID: "art-1", ArtistName: "A", Title: "Off", Source: "mock", SourceID: "r2", Monitored: false, Status: domain.StatusPending})

	if _, _, err := orch.AddArtist(ctx, &domain.Artist{ID: "art-1", SourceID: "art-1", Name: "A", ImageURL: "x"}, true, false); err != nil {
		t.Fatal(err)
	}
	r1, _ := repo.GetAlbum(ctx, "r1")
	r2, _ := repo.GetAlbum(ctx, "r2")
	r3, _ := repo.GetAlbum(ctx, "r3")
	if r1.Status != domain.StatusCompleted || len(r1.Tracks) != 1 {
		t.Errorf("re-index reset completed album: %+v", r1)
	}
	if r2.Monitored {
		t.Errorf("re-index re-monitored an album the user switched off")
	}
	if !r3.Monitored || r3.Status != domain.StatusPending {
		t.Errorf("new release should be monitored+pending: %+v", r3)
	}
}

func TestEnqueueAllMissing_IncludesSingleTrackedAlbums(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	orch, repo := setupTestOrchestrator(t, 1, newMockDownloader(0, block))
	defer orch.StopQueue()
	ctx := context.Background()

	_ = repo.SaveArtist(ctx, &domain.Artist{ID: "solo", Name: "Solo", Source: "mock", SourceID: "solo", Monitored: false})
	_ = repo.SaveAlbum(ctx, &domain.Album{ID: "s1", ArtistID: "solo", ArtistName: "Solo", Title: "Tracked", Source: "mock", SourceID: "s1", Monitored: true, Status: domain.StatusPending})
	_ = repo.SaveAlbum(ctx, &domain.Album{ID: "s2", ArtistID: "solo", ArtistName: "Solo", Title: "Ignored", Source: "mock", SourceID: "s2", Monitored: false, Status: domain.StatusPending})

	_, n, err := orch.EnqueueAllMissingReleases(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the single tracked album to be queued, got %d", n)
	}
}

func TestCleanTitleAndScore(t *testing.T) {
	cases := map[string]string{
		"Song - Remastered 2011":              "Song",
		"Song (feat. Someone)":                "Song",
		"Song [with Other]":                   "Song",
		"Song - Live at Wembley":              "Song",
		"Song - Part 2":                       "Song - Part 2",
		"Maçã de Adão (feat. X) - Radio Edit": "Maçã de Adão",
	}
	for in, want := range cases {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	tr := spotify.Track{Title: "Sério", Artists: []string{"Alcool Club"}, DurationSec: 200}
	good := scoreTrackHit(tr, indexer.TrackHit{Title: "Sério", ArtistName: "Alcool Club", DurationSec: 201})
	bad := scoreTrackHit(tr, indexer.TrackHit{Title: "Something Else", ArtistName: "Other", DurationSec: 300})
	if good < 0.95 || bad >= minTrackMatchScore {
		t.Fatalf("scores off: good=%.2f bad=%.2f", good, bad)
	}
}

func TestPlaylistResolveAndM3U(t *testing.T) {
	orch, repo := setupTestOrchestrator(t, 1, newMockDownloader(0, nil))
	defer orch.StopQueue()
	ctx := context.Background()
	idx := &catalogIndexer{hits: map[string][]indexer.TrackHit{
		"song one": {{Title: "Song One", ArtistName: "Band", DurationSec: 180, AlbumID: "alb-1", AlbumTitle: "Record"}},
		"song two": {{Title: "Totally Different", ArtistName: "Nobody", DurationSec: 500, AlbumID: "alb-9"}},
	}}
	useIndexer(orch, idx)

	tracks, err := resolvePlaylistTracks(ctx, idx, []spotify.Track{
		{Title: "Song One - Remastered", Artists: []string{"Band"}, DurationSec: 181},
		{Title: "Song Two", Artists: []string{"Band"}, DurationSec: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tracks[0].AlbumID != "alb-1" || tracks[1].AlbumID != "" {
		t.Fatalf("resolution wrong: %+v", tracks)
	}

	// Put the release on disk the way a library import would.
	music := orch.MusicPath()
	albumDir := filepath.Join(music, "Band", "Record (2020)")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(albumDir, "01 - Song One.flac"), []byte("x"), 0o644)
	_ = repo.SaveArtist(ctx, &domain.Artist{ID: "b", Name: "Band", Source: "mock", SourceID: "b"})
	_ = repo.SaveAlbum(ctx, &domain.Album{ID: "alb-1", ArtistID: "b", ArtistName: "Band", Title: "Record", Year: 2020, Source: "mock", SourceID: "alb-1",
		Monitored: true, Status: domain.StatusCompleted, Tracks: []domain.Track{{ID: "t1", Title: "Song One", TrackNumber: 1, DiscNumber: 1}}})

	pl := &domain.Playlist{ID: "spotify-x", Source: "spotify", SourceID: "x", Name: "My Mix", Tracks: tracks, LastSyncedAt: time.Now()}
	if err := repo.SavePlaylist(ctx, pl); err != nil {
		t.Fatal(err)
	}
	res, err := orch.WritePlaylistM3U(ctx, pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Found != 1 || res.Missing != 1 {
		t.Fatalf("m3u counts: %+v", res)
	}
	body, err := os.ReadFile(filepath.Join(music, "Playlists", "My Mix.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "../Band/Record (2020)/01 - Song One.flac") {
		t.Fatalf("m3u missing relative track path:\n%s", body)
	}
	saved, _ := repo.GetPlaylist(ctx, pl.ID)
	if saved.M3UPath == "" {
		t.Fatal("m3u path not stored on playlist")
	}

	// Deleting the album folder through the file manager resets the release.
	if n := orch.MarkDeletedPath(ctx, albumDir); n != 1 {
		t.Fatalf("MarkDeletedPath reset %d releases, want 1", n)
	}
	if a, _ := repo.GetAlbum(ctx, "alb-1"); a.Status != domain.StatusPending {
		t.Fatalf("album should be pending after its folder was deleted, got %s", a.Status)
	}
}
