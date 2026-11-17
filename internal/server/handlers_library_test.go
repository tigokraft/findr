package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesEndpoints(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	music := t.TempDir()
	srv.cfg.Library.MusicPath = music
	if err := os.MkdirAll(filepath.Join(music, "Artist", "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(music, "Artist", "Album", "01 - a.flac"), []byte("abc"), 0o644)
	h := srv.httpServer.Handler

	do := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodGet, "/api/files?root=music&path="+url.QueryEscape("Artist/Album"), "")
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var listing struct {
		Entries    []struct{ Name string } `json:"entries"`
		AudioCount int                     `json:"audio_count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listing)
	if listing.AudioCount != 1 || len(listing.Entries) != 1 {
		t.Fatalf("listing: %s", rec.Body)
	}

	if rec := do(http.MethodGet, "/api/files/raw?root=music&path="+url.QueryEscape("Artist/Album/01 - a.flac"), ""); rec.Code != 200 || rec.Body.String() != "abc" {
		t.Fatalf("raw: %d %q", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodPost, "/api/files", `{"action":"rename","root":"music","path":"Artist/Album","name":"Album (2020)"}`); rec.Code != 200 {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodDelete, "/api/files?root=music&path=", ""); rec.Code == 200 {
		t.Fatal("deleting the library root must be refused")
	}
	if rec := do(http.MethodDelete, "/api/files?root=music&path="+url.QueryEscape("Artist/Album (2020)"), ""); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(music, "Artist", "Album (2020)")); !os.IsNotExist(err) {
		t.Fatal("folder still on disk")
	}
	if rec := do(http.MethodGet, "/api/files?root=etc&path=", ""); rec.Code == 200 {
		t.Fatal("unknown root must fail")
	}
}

func TestPlaylistEndpointsValidateInput(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	h := srv.httpServer.Handler

	req := httptest.NewRequest(http.MethodPost, "/api/playlists/preview", strings.NewReader(`{"url":"https://example.com/nope"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not a Spotify playlist") {
		t.Fatalf("expected validation error, got %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/playlists", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
}
