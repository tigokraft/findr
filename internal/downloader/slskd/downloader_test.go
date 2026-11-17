package slskd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/findr-app/findr/internal/domain"
)

func TestSlskdDownloader_Ping(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/application" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "secret-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server":{"isConnected":true}}`))
	}))
	defer ts.Close()

	dl := NewDownloader(Config{
		URL:    ts.URL,
		APIKey: "secret-key",
	})

	if err := dl.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}

	badDl := NewDownloader(Config{
		URL:    ts.URL,
		APIKey: "wrong-key",
	})
	if err := badDl.Ping(context.Background()); err == nil {
		t.Fatalf("expected error for bad API key, got nil")
	}
}

func TestSlskdDownloader_SearchAndDownload(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "slskd_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	downloadDir := filepath.Join(tmpDir, "downloads")
	_ = os.MkdirAll(downloadDir, 0755)

	// Pre-create simulated files that slskd would download into its downloadDir
	peerFolder := filepath.Join(downloadDir, "Daft Punk - Discovery (2001)")
	_ = os.MkdirAll(peerFolder, 0755)
	_ = os.WriteFile(filepath.Join(peerFolder, "01 - One More Time.flac"), []byte("audio1"), 0644)
	_ = os.WriteFile(filepath.Join(peerFolder, "02 - Aerodynamic.flac"), []byte("audio2"), 0644)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/searches":
			if r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"search-123","state":"InProgress"}`))
				return
			}
		case "/api/v0/searches/search-123":
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"state":"Completed","fileCount":2}`))
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusOK)
				return
			}
		case "/api/v0/searches/search-123/responses":
			w.Header().Set("Content-Type", "application/json")
			resp := []SlskdSearchResponse{
				{
					Username:          "musicfan",
					UploadSpeed:       2000000,
					HasFreeUploadSlot: true,
					FileCount:         2,
					Files: []SlskdResponseFile{
						{
							Filename:   `Music\Daft Punk - Discovery (2001)\01 - One More Time.flac`,
							Size:       6,
							SampleRate: 44100,
							BitDepth:   16,
						},
						{
							Filename:   `Music\Daft Punk - Discovery (2001)\02 - Aerodynamic.flac`,
							Size:       6,
							SampleRate: 44100,
							BitDepth:   16,
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return

		case "/api/v0/transfers/downloads/musicfan":
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				userTransfers := SlskdUserTransfers{
					Username: "musicfan",
					Directories: []SlskdDirectoryTransfers{
						{
							Directory: `Music\Daft Punk - Discovery (2001)`,
							FileCount: 2,
							Files: []SlskdTransferFile{
								{
									ID:               "tr-1",
									Filename:         `Music\Daft Punk - Discovery (2001)\01 - One More Time.flac`,
									Size:             6,
									BytesTransferred: 6,
									State:            "Completed, Succeeded",
								},
								{
									ID:               "tr-2",
									Filename:         `Music\Daft Punk - Discovery (2001)\02 - Aerodynamic.flac`,
									Size:             6,
									BytesTransferred: 6,
									State:            "Completed, Succeeded",
								},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(userTransfers)
				return
			}
		}

		http.NotFound(w, r)
	}))
	defer ts.Close()

	dl := NewDownloader(Config{
		URL:         ts.URL,
		APIKey:      "test-key",
		DownloadDir: downloadDir,
	})

	album := &domain.Album{
		ID:          "alb-discovery",
		Title:       "Discovery",
		ArtistName:  "Daft Punk",
		TotalTracks: 2,
	}

	// 1. Test Search
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results, err := dl.Search(ctx, album)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected at least 1 search result, got 0")
	}
	if results[0].TotalTracks != 2 {
		t.Errorf("expected 2 total tracks, got %d", results[0].TotalTracks)
	}
	if results[0].Format != domain.QualityFLAC16 {
		t.Errorf("expected FLAC16 format, got %s", results[0].Format)
	}

	// 2. Test Download into staging directory
	stagingDir := filepath.Join(tmpDir, "staging")
	job, err := dl.Download(ctx, album, stagingDir, func(pct float64, msg string) {})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	if job.Status != domain.StatusDownloaded {
		t.Errorf("expected StatusDownloaded, got %s", job.Status)
	}
	if len(job.DownloadedFiles) != 2 {
		t.Errorf("expected 2 downloaded files, got %d", len(job.DownloadedFiles))
	}

	// Verify files exist in stagingDir
	for _, f := range job.DownloadedFiles {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected file %s to exist in stagingDir, got error: %v", f, err)
		}
	}
}

// A peer that times out on enqueue (slskd 500 "wait timed out") must not fail
// the album: the next ranked peer is tried.
func TestSlskdDownloader_EnqueueFailureTriesNextPeer(t *testing.T) {
	tmpDir := t.TempDir()
	downloadDir := filepath.Join(tmpDir, "downloads")
	peerFolder := filepath.Join(downloadDir, "Currents")
	_ = os.MkdirAll(peerFolder, 0755)
	_ = os.WriteFile(filepath.Join(peerFolder, "01 - Let It Happen.flac"), []byte("audio1"), 0644)

	file := SlskdResponseFile{Filename: `Music\Currents\01 - Let It Happen.flac`, Size: 6, SampleRate: 44100, BitDepth: 16}
	deadEnqueues := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/searches":
			_, _ = w.Write([]byte(`{"id":"s1","state":"InProgress"}`))
			return
		case "/api/v0/searches/s1":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"state":"Completed"}`))
			}
			return
		case "/api/v0/searches/s1/responses":
			_ = json.NewEncoder(w).Encode([]SlskdSearchResponse{
				{Username: "deadpeer", HasFreeUploadSlot: true, Files: []SlskdResponseFile{file}},
				{Username: "goodpeer", HasFreeUploadSlot: false, QueueLength: 3, Files: []SlskdResponseFile{file}},
			})
			return
		case "/api/v0/transfers/downloads/deadpeer":
			if r.Method == http.MethodPost {
				deadEnqueues++
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`"The wait timed out after 5000 milliseconds"`))
			}
			return
		case "/api/v0/transfers/downloads/goodpeer":
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
				return
			}
			_ = json.NewEncoder(w).Encode(SlskdUserTransfers{
				Username: "goodpeer",
				Directories: []SlskdDirectoryTransfers{{
					Directory: `Music\Currents`,
					Files: []SlskdTransferFile{{
						ID: "t1", Filename: file.Filename, Size: 6, BytesTransferred: 6, State: "Completed, Succeeded",
					}},
				}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	dl := NewDownloader(Config{URL: ts.URL, APIKey: "k", DownloadDir: downloadDir})
	album := &domain.Album{ID: "a1", Title: "Currents", ArtistName: "Tame Impala", TotalTracks: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	job, err := dl.Download(ctx, album, filepath.Join(tmpDir, "staging"), nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if deadEnqueues != 1 {
		t.Errorf("expected the dead peer to be tried once first, got %d enqueue attempts", deadEnqueues)
	}
	if job.Status != domain.StatusDownloaded || len(job.DownloadedFiles) != 1 {
		t.Errorf("expected 1 file downloaded from goodpeer, got status %s files %v", job.Status, job.DownloadedFiles)
	}
}
