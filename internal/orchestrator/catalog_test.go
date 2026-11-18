package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
)

type threeTrackIndexer struct{ mockIndexer }

func (i *threeTrackIndexer) GetAlbumDetails(ctx context.Context, albumID string) (*domain.Album, error) {
	return &domain.Album{
		ID: albumID, SourceID: albumID, ArtistID: "art-1", ArtistName: "Band", Title: "Three",
		Tracks: []domain.Track{
			{ID: "t1", Title: "One", TrackNumber: 1},
			{ID: "t2", Title: "Two", TrackNumber: 2},
			{ID: "t3", Title: "Three", TrackNumber: 3},
		},
		TotalTracks: 3,
	}, nil
}

// recordingDownloader captures the tracklist it was asked for, then fails so
// the pipeline stops before verification.
type recordingDownloader struct{ got []domain.Track }

func (r *recordingDownloader) Name() string { return "rec" }
func (r *recordingDownloader) Search(ctx context.Context, a *domain.Album) ([]downloader.SearchResult, error) {
	return nil, nil
}
func (r *recordingDownloader) Download(ctx context.Context, a *domain.Album, dir string, p downloader.ProgressFunc) (*domain.DownloadJob, error) {
	r.got = append([]domain.Track(nil), a.Tracks...)
	return nil, errors.New("stop here")
}

func TestAddRelease_PicksMergeAndCollapseToWholeRelease(t *testing.T) {
	orch, repo := setupTestOrchestrator(t, 1, newMockDownloader(0, nil))
	defer orch.StopQueue()
	useIndexer(orch, &threeTrackIndexer{})
	ctx := context.Background()

	a, err := orch.AddRelease(ctx, AddReleaseRequest{SourceID: "alb", TrackIDs: []string{"t2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.WantedTrackIDs, []string{"t2"}) || len(a.Tracks) != 3 {
		t.Fatalf("want one picked track and the full tracklist kept, got %+v", a)
	}
	stored, _ := repo.GetAlbum(ctx, "alb")
	if !reflect.DeepEqual(stored.WantedTrackIDs, []string{"t2"}) {
		t.Fatalf("picks not persisted: %+v", stored.WantedTrackIDs)
	}

	// A later pick for a completed partial release adds to it and re-queues.
	stored.Status = domain.StatusCompleted
	_ = repo.SaveAlbum(ctx, stored)
	a, _ = orch.AddRelease(ctx, AddReleaseRequest{SourceID: "alb", TrackIDs: []string{"t3"}})
	if !reflect.DeepEqual(a.WantedTrackIDs, []string{"t2", "t3"}) || a.Status != domain.StatusPending {
		t.Fatalf("expected merged picks and pending status, got %v %s", a.WantedTrackIDs, a.Status)
	}

	// Picking the last missing track is the whole release.
	a, _ = orch.AddRelease(ctx, AddReleaseRequest{SourceID: "alb", TrackIDs: []string{"t1"}})
	if a.IsPartial() {
		t.Fatalf("all tracks picked should mean whole release, got %v", a.WantedTrackIDs)
	}

	// Picking a track of a release already fully on disk changes nothing.
	a.Status = domain.StatusCompleted
	_ = repo.SaveAlbum(ctx, a)
	a, _ = orch.AddRelease(ctx, AddReleaseRequest{SourceID: "alb", TrackIDs: []string{"t1"}})
	if a.IsPartial() || a.Status != domain.StatusCompleted {
		t.Fatalf("complete release should stay complete: %v %s", a.WantedTrackIDs, a.Status)
	}
}

func TestPipeline_DownloadsOnlyPickedTracks(t *testing.T) {
	orch, repo := setupTestOrchestrator(t, 1, newMockDownloader(0, nil))
	defer orch.StopQueue()
	rec := &recordingDownloader{}
	orch.mu.Lock()
	orch.indexer = &threeTrackIndexer{}
	orch.downloader = rec
	orch.mu.Unlock()
	ctx := context.Background()

	if _, err := orch.AddRelease(ctx, AddReleaseRequest{SourceID: "alb", TrackIDs: []string{"t3", "t1"}}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = orch.ProcessAlbumPipeline(ctx, "alb", nil)
	if len(rec.got) != 2 || rec.got[0].ID != "t1" || rec.got[1].ID != "t3" {
		t.Fatalf("downloader should get t1,t3 in album order, got %+v", rec.got)
	}
	stored, _ := repo.GetAlbum(ctx, "alb")
	if len(stored.Tracks) != 3 {
		t.Fatalf("stored album lost its tracklist: %d tracks", len(stored.Tracks))
	}
}

func TestLibraryID_PrefixesOtherCatalogs(t *testing.T) {
	orch, _ := setupTestOrchestrator(t, 1, newMockDownloader(0, nil))
	defer orch.StopQueue()
	useIndexer(orch, &namedIndexer{name: "deezer"})
	if got := orch.libraryID("deezer", "42"); got != "42" {
		t.Errorf("default catalog keeps bare ID, got %q", got)
	}
	if got := orch.libraryID("qobuz", "42"); got != "qobuz-42" {
		t.Errorf("other catalog is prefixed, got %q", got)
	}
	if orch.indexerFor("qobuz").Name() != "qobuz" || orch.indexerFor("").Name() != "deezer" {
		t.Errorf("indexerFor routes by catalog")
	}
}

type namedIndexer struct {
	mockIndexer
	name string
}

func (n *namedIndexer) Name() string { return n.name }
