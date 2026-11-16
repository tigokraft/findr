package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader/qobuz"
	"github.com/findr-app/findr/internal/importer"
	"github.com/findr-app/findr/internal/indexer/deezer"
	"github.com/findr-app/findr/internal/orchestrator"
	"github.com/findr-app/findr/internal/storage"
	"github.com/findr-app/findr/internal/verifier"
)

func setupTestServer(t *testing.T) (*Server, func()) {
	tmpDB := "/tmp/findr_server_test.db"
	_ = os.Remove(tmpDB)

	repo, err := storage.NewSQLiteRepository(tmpDB)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}

	cfg := config.DefaultConfig()
	idx := deezer.NewClient()
	dl := qobuz.NewDownloader(qobuz.Config{AppID: "305874112"})
	vr := verifier.NewVerifier(verifier.Config{DurationToleranceSec: 4, MinConfidenceScore: 0.85})
	imp := importer.NewImporter(importer.Config{MusicPath: "/tmp/music"})

	orch := orchestrator.NewOrchestrator(cfg, repo, idx, dl, vr, imp, nil)
	srv := NewServer(cfg, orch, repo, nil)

	cleanup := func() {
		_ = repo.Close()
		_ = os.Remove(tmpDB)
	}

	return srv, cleanup
}

func TestHealthzEndpoint(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

func TestStatusEndpoint(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode status: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("expected running status, got %v", resp["status"])
	}
}

func TestArtistsEndpoint(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	// Pre-seed an artist
	_ = srv.repo.SaveArtist(context.Background(), &domain.Artist{
		ID:        "100",
		Name:      "Daft Punk",
		Source:    "deezer",
		SourceID:  "27",
		Monitored: true,
	})

	// GET /api/artists
	req := httptest.NewRequest(http.MethodGet, "/api/artists", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var artists []domain.Artist
	if err := json.NewDecoder(w.Body).Decode(&artists); err != nil {
		t.Fatalf("decode artists failed: %v", err)
	}
	if len(artists) != 1 || artists[0].Name != "Daft Punk" {
		t.Errorf("unexpected artists: %+v", artists)
	}

	// POST /api/artists with empty query should fail with 400
	badReq := httptest.NewRequest(http.MethodPost, "/api/artists", bytes.NewReader([]byte(`{"query":""}`)))
	wBad := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wBad, badReq)
	if wBad.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", wBad.Code)
	}
}
