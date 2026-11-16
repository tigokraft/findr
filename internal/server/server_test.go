package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader/qobuz"
	"github.com/findr-app/findr/internal/importer"
	"github.com/findr-app/findr/internal/indexer/deezer"
	"github.com/findr-app/findr/internal/navidrome"
	"github.com/findr-app/findr/internal/orchestrator"
	"github.com/findr-app/findr/internal/storage"
	"github.com/findr-app/findr/internal/verifier"
)

func setupTestServer(t *testing.T) (*Server, func()) {
	tmpDir := t.TempDir()
	tmpDB := filepath.Join(tmpDir, "findr_server_test.db")
	tmpCfgPath := filepath.Join(tmpDir, "config.yaml")

	repo, err := storage.NewSQLiteRepository(tmpDB)
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Database.Path = tmpDB
	_ = config.Save(tmpCfgPath, cfg)

	idx := deezer.NewClient()
	dl := qobuz.NewDownloader(qobuz.Config{AppID: "798273057"})
	vr := verifier.NewVerifier(verifier.Config{DurationToleranceSec: 4, MinConfidenceScore: 0.85})
	imp := importer.NewImporter(importer.Config{MusicPath: filepath.Join(tmpDir, "music")})

	orch := orchestrator.NewOrchestrator(cfg, repo, idx, dl, vr, imp, nil)
	srv := NewServer(cfg, tmpCfgPath, orch, repo, nil)

	cleanup := func() {
		if orch != nil {
			orch.StopQueue()
		}
		_ = repo.Close()
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
	if _, ok := resp["disks"]; !ok {
		t.Errorf("expected disks in status response")
	}
}

func TestDisksEndpoint(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/disks", nil)
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var disks []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&disks); err != nil {
		t.Fatalf("failed to decode disks: %v", err)
	}
	if len(disks) == 0 {
		t.Errorf("expected at least one disk entry")
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

	// DELETE /api/artists/100
	delReq := httptest.NewRequest(http.MethodDelete, "/api/artists/100", nil)
	wDel := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wDel, delReq)
	if wDel.Code != http.StatusOK {
		t.Errorf("expected 200 on delete, got %d", wDel.Code)
	}

	// Verify deletion
	a, _ := srv.repo.GetArtist(context.Background(), "100")
	if a != nil {
		t.Errorf("expected artist 100 to be deleted")
	}
}

func TestConfigEndpoints(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	// GET /api/config
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var cfg config.Config
	if err := json.NewDecoder(w.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}

	// Update config via POST /api/config
	cfg.Library.FolderFormat = "{Artist} - {Album} ({Year})"
	cfg.Export.WriteNFO = true
	cfg.Import.DeleteAfterImport = false
	cfg.Navidrome.Enabled = true
	cfg.Navidrome.URL = "http://navidrome-test:4533"

	body, _ := json.Marshal(cfg)
	postReq := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	wPost := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wPost, postReq)

	if wPost.Code != http.StatusOK {
		t.Fatalf("expected 200 on config update, got %d: %s", wPost.Code, wPost.Body.String())
	}

	// Verify srv.cfg was updated live
	if srv.cfg.Library.FolderFormat != "{Artist} - {Album} ({Year})" {
		t.Errorf("folder format not updated in memory")
	}
	if !srv.cfg.Export.WriteNFO {
		t.Errorf("export.write_nfo not updated in memory")
	}

	// GET /api/config/raw
	rawReq := httptest.NewRequest(http.MethodGet, "/api/config/raw", nil)
	wRaw := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wRaw, rawReq)
	if wRaw.Code != http.StatusOK {
		t.Errorf("expected 200 on raw config, got %d", wRaw.Code)
	}
	if !bytes.Contains(wRaw.Body.Bytes(), []byte("folder_format")) {
		t.Errorf("expected raw yaml to contain folder_format")
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "[]\n" {
		t.Errorf("expected empty array for blank query, got %s", w.Body.String())
	}
}

func TestTestNavidromeEndpoint(t *testing.T) {
	// Mock Navidrome Subsonic server
	naviMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"subsonic-response": {"status": "ok", "version": "1.16.0"}}`))
	}))
	defer naviMock.Close()

	srv, cleanup := setupTestServer(t)
	defer cleanup()

	payload := map[string]interface{}{
		"url":        naviMock.URL,
		"username":   "admin",
		"password":   "testpass",
		"token_auth": true,
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/config/test-navidrome", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode test result: %v", err)
	}
	if resp["success"] != true {
		t.Errorf("expected success true, got %v", resp)
	}
}

func TestDashboardServed(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	paths := []string{"/", "/search", "/settings", "/activity", "/queue", "/history", "/system", "/artist", "/artist/123"}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for path %s, got %d", p, w.Code)
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("findr")) {
			t.Errorf("expected page to contain 'findr' on %s", p)
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("Search & Add")) {
			t.Errorf("expected page to contain 'Search & Add' on %s", p)
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("id=\"view-artist\"")) {
			t.Errorf("expected page to contain dedicated artist view 'id=\"view-artist\"' on %s", p)
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("openArtistPage")) {
			t.Errorf("expected page to contain 'openArtistPage' on %s", p)
		}
	}
}

func TestQueueAndHistoryEndpoints(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Seed an album and an active job + a completed job + a failed job
	_ = srv.repo.SaveAlbum(ctx, &domain.Album{
		ID:         "alb-101",
		ArtistName: "Daft Punk",
		Title:      "Discovery",
		Year:       2001,
	})

	activeJob := &domain.DownloadJob{
		ID:            "job-active-1",
		AlbumID:       "alb-101",
		Downloader:    "qobuz",
		TargetQuality: domain.QualityFLAC2419,
		Status:        domain.StatusDownloading,
		Progress:      45.0,
	}
	_ = srv.repo.SaveDownloadJob(ctx, activeJob)

	failedJob := &domain.DownloadJob{
		ID:            "job-failed-1",
		AlbumID:       "alb-101",
		Downloader:    "qobuz",
		TargetQuality: domain.QualityFLAC16,
		Status:        domain.StatusFailed,
		ErrorMessage:  "album not found on Qobuz catalog",
	}
	_ = srv.repo.SaveDownloadJob(ctx, failedJob)

	// 2. Test GET /api/queue
	reqQ := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	wQ := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wQ, reqQ)

	if wQ.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/queue, got %d", wQ.Code)
	}

	var queueJobs []EnrichedDownloadJob
	if err := json.NewDecoder(wQ.Body).Decode(&queueJobs); err != nil {
		t.Fatalf("failed to decode queue: %v", err)
	}
	if len(queueJobs) != 1 {
		t.Fatalf("expected 1 active job in queue, got %d", len(queueJobs))
	}
	if queueJobs[0].AlbumTitle != "Discovery" || queueJobs[0].ArtistName != "Daft Punk" {
		t.Errorf("expected enriched Discovery Daft Punk, got %s by %s", queueJobs[0].AlbumTitle, queueJobs[0].ArtistName)
	}
	if queueJobs[0].Progress != 45.0 {
		t.Errorf("expected progress 45, got %v", queueJobs[0].Progress)
	}

	// 3. Test GET /api/history
	reqH := httptest.NewRequest(http.MethodGet, "/api/history?limit=10", nil)
	wH := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wH, reqH)

	if wH.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/history, got %d", wH.Code)
	}

	var histJobs []EnrichedDownloadJob
	if err := json.NewDecoder(wH.Body).Decode(&histJobs); err != nil {
		t.Fatalf("failed to decode history: %v", err)
	}
	if len(histJobs) != 2 {
		t.Fatalf("expected 2 history jobs, got %d", len(histJobs))
	}

	// 4. Test POST /api/history/clear
	reqC := httptest.NewRequest(http.MethodPost, "/api/history/clear", nil)
	wC := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wC, reqC)

	if wC.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/history/clear, got %d", wC.Code)
	}

	// Verify history is cleared (non-active jobs deleted)
	histAfter, _ := srv.orch.GetDownloadHistory(ctx, 10)
	for _, j := range histAfter {
		if j.Status == domain.StatusFailed || j.Status == domain.StatusCompleted {
			t.Errorf("expected completed/failed job to be cleared, still found %s with status %s", j.ID, j.Status)
		}
	}
}

func TestQueueStatusAndCancelEndpoints(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	// 1. GET /api/queue/status
	req := httptest.NewRequest(http.MethodGet, "/api/queue/status", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/queue/status, got %d", w.Code)
	}

	var status map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("failed to decode queue status: %v", err)
	}

	if max, ok := status["max_concurrent"].(float64); !ok || max < 1 {
		t.Errorf("expected max_concurrent >= 1, got %v", status["max_concurrent"])
	}

	// 2. POST /api/queue/cancel with empty album_id
	cancelBody, _ := json.Marshal(map[string]string{"album_id": ""})
	reqCancel := httptest.NewRequest(http.MethodPost, "/api/queue/cancel", bytes.NewReader(cancelBody))
	wCancel := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wCancel, reqCancel)

	if wCancel.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty album_id cancel, got %d", wCancel.Code)
	}

	// 3. POST /api/queue/cancel with nonexistent album_id
	cancelBodyNon, _ := json.Marshal(map[string]string{"album_id": "nonexistent"})
	reqCancelNon := httptest.NewRequest(http.MethodPost, "/api/queue/cancel", bytes.NewReader(cancelBodyNon))
	wCancelNon := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wCancelNon, reqCancelNon)

	if wCancelNon.Code != http.StatusOK {
		t.Errorf("expected 200 for nonexistent album_id cancel, got %d", wCancelNon.Code)
	}
}

func TestBatchAlbumActions(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	ctx := context.Background()

	// Seed artist
	art := &domain.Artist{
		ID:        "art-batch-srv",
		Name:      "Server Batch Artist",
		Source:    "deezer",
		SourceID:  "src-batch-srv",
		Monitored: true,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	_ = srv.repo.SaveArtist(ctx, art)

	// Seed 2 albums
	alb1 := &domain.Album{
		ID:          "alb-batch-1",
		ArtistID:    "art-batch-srv",
		SourceID:    "s-b-1",
		Title:       "Batch Release 1",
		ArtistName:  "Server Batch Artist",
		ReleaseType: domain.ReleaseTypeAlbum,
		Monitored:   false,
		Status:      domain.DownloadStatus("wanted"),
	}
	alb2 := &domain.Album{
		ID:          "alb-batch-2",
		ArtistID:    "art-batch-srv",
		SourceID:    "s-b-2",
		Title:       "Batch Release 2",
		ArtistName:  "Server Batch Artist",
		ReleaseType: domain.ReleaseTypeAlbum,
		Monitored:   false,
		Status:      domain.DownloadStatus("wanted"),
	}
	_ = srv.repo.SaveAlbum(ctx, alb1)
	_ = srv.repo.SaveAlbum(ctx, alb2)

	// 1. Batch Monitor
	monitorPayload, _ := json.Marshal(map[string]interface{}{
		"action":    "monitor",
		"album_ids": []string{"alb-batch-1", "alb-batch-2"},
	})
	reqMon := httptest.NewRequest(http.MethodPost, "/api/albums/batch", bytes.NewReader(monitorPayload))
	wMon := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wMon, reqMon)

	if wMon.Code != http.StatusOK {
		t.Fatalf("expected 200 for batch monitor, got %d: %s", wMon.Code, wMon.Body.String())
	}

	fresh1, _ := srv.repo.GetAlbum(ctx, "alb-batch-1")
	fresh2, _ := srv.repo.GetAlbum(ctx, "alb-batch-2")
	if !fresh1.Monitored || !fresh2.Monitored {
		t.Errorf("expected both albums monitored, got alb1=%v, alb2=%v", fresh1.Monitored, fresh2.Monitored)
	}

	// 2. Batch Unmonitor
	unmonitorPayload, _ := json.Marshal(map[string]interface{}{
		"action":    "unmonitor",
		"album_ids": []string{"alb-batch-1"},
	})
	reqUnmon := httptest.NewRequest(http.MethodPost, "/api/albums/batch", bytes.NewReader(unmonitorPayload))
	wUnmon := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wUnmon, reqUnmon)

	if wUnmon.Code != http.StatusOK {
		t.Fatalf("expected 200 for batch unmonitor, got %d", wUnmon.Code)
	}

	fresh1After, _ := srv.repo.GetAlbum(ctx, "alb-batch-1")
	if fresh1After.Monitored {
		t.Errorf("expected alb-batch-1 unmonitored")
	}

	// 3. Batch Download
	dlPayload, _ := json.Marshal(map[string]interface{}{
		"action":    "download",
		"album_ids": []string{"alb-batch-1", "alb-batch-2"},
	})
	reqDL := httptest.NewRequest(http.MethodPost, "/api/albums/batch", bytes.NewReader(dlPayload))
	wDL := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wDL, reqDL)

	if wDL.Code != http.StatusOK {
		t.Fatalf("expected 200 for batch download, got %d: %s", wDL.Code, wDL.Body.String())
	}
}

func TestImportScanUploadAndExecute(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	// 1. Test POST /api/import/scan
	scanDir := t.TempDir()
	dummyFile1 := filepath.Join(scanDir, "01 - Test Song.flac")
	dummyFile2 := filepath.Join(scanDir, "02 - Another Song.mp3")
	_ = os.WriteFile(dummyFile1, []byte("dummy flac"), 0644)
	_ = os.WriteFile(dummyFile2, []byte("dummy mp3"), 0644)

	scanPayload, _ := json.Marshal(map[string]string{"path": scanDir})
	reqScan := httptest.NewRequest(http.MethodPost, "/api/import/scan", bytes.NewReader(scanPayload))
	wScan := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wScan, reqScan)

	if wScan.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/import/scan, got %d: %s", wScan.Code, wScan.Body.String())
	}

	var scanResp map[string]interface{}
	if err := json.NewDecoder(wScan.Body).Decode(&scanResp); err != nil {
		t.Fatalf("failed to decode scan response: %v", err)
	}
	files, ok := scanResp["files"].([]interface{})
	if !ok || len(files) != 2 {
		t.Fatalf("expected 2 detected files in scan, got %v", scanResp["files"])
	}

	// 2. Test POST /api/import/upload
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("files", "uploaded_track.flac")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	_, _ = part.Write([]byte("fake flac audio stream"))
	_ = writer.Close()

	reqUpload := httptest.NewRequest(http.MethodPost, "/api/import/upload", body)
	reqUpload.Header.Set("Content-Type", writer.FormDataContentType())
	wUpload := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wUpload, reqUpload)

	if wUpload.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/import/upload, got %d: %s", wUpload.Code, wUpload.Body.String())
	}

	var uploadResp map[string]interface{}
	if err := json.NewDecoder(wUpload.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("failed to decode upload response: %v", err)
	}
	upFiles, ok := uploadResp["files"].([]interface{})
	if !ok || len(upFiles) != 1 {
		t.Fatalf("expected 1 uploaded file, got %v", uploadResp["files"])
	}

	// 3. Test POST /api/import/execute
	execPayload, _ := json.Marshal(map[string]interface{}{
		"artist_name":  "Imported Artist",
		"album_title": "Imported Album",
		"release_type": "album",
		"year":         2024,
		"move_or_copy": "copy",
		"files": []map[string]interface{}{
			{
				"file_path":    dummyFile1,
				"file_name":    "01 - Test Song.flac",
				"title":        "Test Song",
				"track_number": 1,
			},
		},
	})

	reqExec := httptest.NewRequest(http.MethodPost, "/api/import/execute", bytes.NewReader(execPayload))
	wExec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wExec, reqExec)

	if wExec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/import/execute, got %d: %s", wExec.Code, wExec.Body.String())
	}

	var execResp map[string]interface{}
	if err := json.NewDecoder(wExec.Body).Decode(&execResp); err != nil {
		t.Fatalf("failed to decode execute response: %v", err)
	}
	if execResp["status"] != "success" {
		t.Errorf("expected status 'success', got %v", execResp["status"])
	}
	if dest, ok := execResp["destination"].(string); !ok || dest == "" {
		t.Errorf("expected destination string in response, got %v", execResp["destination"])
	}
}

func TestScanEndpoint(t *testing.T) {
	// 1. When navidrome is disabled (default in setupTestServer)
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	reqDisabled := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	wDisabled := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wDisabled, reqDisabled)

	if wDisabled.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when navidrome disabled, got %d", wDisabled.Code)
	}

	var disabledResp map[string]interface{}
	if err := json.NewDecoder(wDisabled.Body).Decode(&disabledResp); err != nil {
		t.Fatalf("decode disabled scan response failed: %v", err)
	}
	if disabledResp["error"] != "navidrome integration not enabled" {
		t.Errorf("expected 'navidrome integration not enabled', got %v", disabledResp["error"])
	}

	// 2. Mock Navidrome server responding successfully
	naviMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"subsonic-response": {"status": "ok", "version": "1.16.0", "scanStatus": {"scanning": true, "count": 100}}}`))
	}))
	defer naviMock.Close()

	srv.mu.Lock()
	srv.navidrome = navidrome.NewClient(navidrome.Config{
		URL:       naviMock.URL,
		Username:  "admin",
		Password:  "password",
		TokenAuth: false,
	})
	srv.mu.Unlock()

	reqScan := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	wScan := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wScan, reqScan)

	if wScan.Code != http.StatusOK {
		t.Fatalf("expected 200 for /api/scan, got %d: %s", wScan.Code, wScan.Body.String())
	}

	var scanResp map[string]interface{}
	if err := json.NewDecoder(wScan.Body).Decode(&scanResp); err != nil {
		t.Fatalf("decode scan response: %v", err)
	}
	if scanResp["scanning"] != true {
		t.Errorf("expected scanning true, got %v", scanResp)
	}

	// 3. Mock Navidrome server returning error (e.g. invalid auth)
	naviMockErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"subsonic-response": {"status": "failed", "error": {"code": 40, "message": "Wrong username or password"}}}`))
	}))
	defer naviMockErr.Close()

	srv.mu.Lock()
	srv.navidrome = navidrome.NewClient(navidrome.Config{
		URL:       naviMockErr.URL,
		Username:  "admin",
		Password:  "wrongpass",
		TokenAuth: false,
	})
	srv.mu.Unlock()

	reqScanErr := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	wScanErr := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(wScanErr, reqScanErr)

	if wScanErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for failed /api/scan, got %d: %s", wScanErr.Code, wScanErr.Body.String())
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(wScanErr.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error scan response: %v", err)
	}
	if errResp["error"] != "navidrome error: Wrong username or password" {
		t.Errorf("expected navidrome error message, got %v", errResp["error"])
	}
}
