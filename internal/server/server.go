package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
	qobuzdl "github.com/findr-app/findr/internal/downloader/qobuz"
	slskddl "github.com/findr-app/findr/internal/downloader/slskd"
	"github.com/findr-app/findr/internal/importer"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/indexer/deezer"
	qobuzidx "github.com/findr-app/findr/internal/indexer/qobuz"
	"github.com/findr-app/findr/internal/navidrome"
	"github.com/findr-app/findr/internal/orchestrator"
	"github.com/findr-app/findr/internal/storage"
	"github.com/findr-app/findr/internal/system"
	"github.com/findr-app/findr/internal/verifier"
	"gopkg.in/yaml.v3"
)

// Server provides HTTP REST APIs and Web UI for findr.
type Server struct {
	cfg        *config.Config
	configPath string
	orch       *orchestrator.Orchestrator
	repo       storage.Repository
	navidrome  *navidrome.Client
	httpServer *http.Server
	startTime  time.Time
	mu         sync.RWMutex
}

// NewServer initializes a new Server instance.
func NewServer(cfg *config.Config, configPath string, orch *orchestrator.Orchestrator, repo storage.Repository, navi *navidrome.Client) *Server {
	if configPath == "" {
		configPath = "config.yaml"
	}
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	s := &Server{
		cfg:        cfg,
		configPath: configPath,
		orch:       orch,
		repo:       repo,
		navidrome:  navi,
		startTime:  time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/disks", s.handleDisks)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/search/preview", s.handleSearchPreview)
	mux.HandleFunc("/api/search/albums", s.handleSearchAlbums)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/files/roots", s.handleFileRoots)
	mux.HandleFunc("/api/files/raw", s.handleFileRaw)
	mux.HandleFunc("/api/playlists", s.handlePlaylists)
	mux.HandleFunc("/api/playlists/", s.handlePlaylistDetail)
	mux.HandleFunc("/api/artists", s.handleArtists)
	mux.HandleFunc("/api/artists/download-all", s.handleDownloadAllArtists)
	mux.HandleFunc("/api/artists/", s.handleArtistDetail)
	mux.HandleFunc("/api/albums", s.handleAlbums)
	mux.HandleFunc("/api/albums/", s.handleAlbumActions)
	mux.HandleFunc("/api/albums/batch", s.handleAlbumBatch)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/queue/status", s.handleQueueStatus)
	mux.HandleFunc("/api/queue/cancel", s.handleQueueCancel)
	mux.HandleFunc("/api/import/upload", s.handleImportUpload)
	mux.HandleFunc("/api/import/scan", s.handleImportScan)
	mux.HandleFunc("/api/import/execute", s.handleImportExecute)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/history/clear", s.handleHistoryClear)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/config/raw", s.handleConfigRaw)
	mux.HandleFunc("/api/config/test-navidrome", s.handleTestNavidrome)
	mux.HandleFunc("/api/config/test-slskd", s.handleTestSlskd)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/library/scan", s.handleLibraryScan)
	mux.HandleFunc("/", s.handleDashboard)

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	return s
}

// Start runs the HTTP server and the background sync scheduler.
func (s *Server) Start(ctx context.Context) error {
	go s.runBackgroundScheduler(ctx)

	errChan := make(chan error, 1)
	go func() {
		fmt.Printf("[findr] HTTP server listening on http://%s\n", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}

func (s *Server) runBackgroundScheduler(ctx context.Context) {
	// Auto-import existing library artists shortly after startup
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}

		s.mu.RLock()
		musicPath := s.cfg.Library.MusicPath
		s.mu.RUnlock()

		if strings.TrimSpace(musicPath) != "" {
			fmt.Printf("[findr Library Sync] Starting automatic library artist import from '%s'...\n", musicPath)
			summary, err := s.orch.ImportArtistsFromLibrary(ctx, func(curr, total int, artist string) {
				if curr%10 == 0 || curr == total {
					fmt.Printf("[findr Library Sync] [%d/%d] Processing %s...\n", curr, total, artist)
				}
			})
			if err != nil {
				fmt.Printf("[findr Library Sync ERROR] %v\n", err)
			} else {
				fmt.Printf("[findr Library Sync SUCCESS] Found %d artist folders: imported %d new artists, reconciled %d existing, matched %d albums\n",
					summary.TotalFoldersFound, summary.ArtistsImported, summary.ArtistsReconciled, summary.AlbumsMatched)
				// Trigger Navidrome scan if configured
				s.mu.RLock()
				navi := s.navidrome
				s.mu.RUnlock()
				if navi != nil {
					_, _ = navi.StartScan(ctx, false)
				}
			}
		}
	}()

	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncMonitoredArtists(ctx)
			s.orch.SyncMonitoredPlaylists(ctx)
		}
	}
}

func (s *Server) syncMonitoredArtists(ctx context.Context) {
	artists, err := s.repo.ListArtists(ctx)
	if err != nil {
		fmt.Printf("[Scheduler] Error listing artists: %v\n", err)
		return
	}

	for _, a := range artists {
		if !a.Monitored {
			continue
		}
		fmt.Printf("[Scheduler] Checking updates for artist: %s\n", a.Name)
		_, _, _ = s.orch.SearchAndAddArtist(ctx, a.Name)
	}
}

func (s *Server) reloadComponents(newCfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Recreate indexer
	var idx indexer.Indexer
	switch newCfg.Indexer.DefaultProvider {
	case "qobuz":
		idx = qobuzidx.NewClient(newCfg.Indexer.Qobuz.AppID)
	default:
		idx = deezer.NewClient()
	}

	// 2. Recreate downloaders with fallback cascade
	var qobuzDownloader *qobuzdl.Downloader
	if newCfg.Downloader.Qobuz.Enabled || newCfg.Downloader.Qobuz.UserToken != "" {
		qobuzDownloader = qobuzdl.NewDownloader(qobuzdl.Config{
			AppID:         newCfg.Downloader.Qobuz.AppID,
			AppSecret:     newCfg.Downloader.Qobuz.AppSecret,
			Username:      newCfg.Downloader.Qobuz.Username,
			Password:      newCfg.Downloader.Qobuz.Password,
			UserToken:     newCfg.Downloader.Qobuz.UserToken,
			TargetQuality: domain.AudioQuality(newCfg.Downloader.Qobuz.TargetQuality),
		})
	}

	var slskdDownloader *slskddl.Downloader
	if newCfg.Downloader.Slskd.Enabled || newCfg.Downloader.Slskd.URL != "" {
		slskdDownloader = slskddl.NewDownloader(slskddl.Config{
			URL:         newCfg.Downloader.Slskd.URL,
			APIKey:      newCfg.Downloader.Slskd.APIKey,
			DownloadDir: newCfg.Downloader.Slskd.DownloadDir,
		})
	}

	// Link slskd as track fallback for qobuz when a track is missing or unstreamable
	if qobuzDownloader != nil && slskdDownloader != nil {
		qobuzDownloader.SetTrackFallback(slskdDownloader)
	}

	var dl downloader.Downloader
	if newCfg.Downloader.Preferred == "slskd" {
		if slskdDownloader != nil && qobuzDownloader != nil {
			dl = downloader.NewFallbackDownloader(slskdDownloader, qobuzDownloader)
		} else if slskdDownloader != nil {
			dl = slskdDownloader
		} else {
			dl = qobuzDownloader
		}
	} else {
		if qobuzDownloader != nil && slskdDownloader != nil {
			dl = downloader.NewFallbackDownloader(qobuzDownloader, slskdDownloader)
		} else if qobuzDownloader != nil {
			dl = qobuzDownloader
		} else {
			dl = slskdDownloader
		}
	}

	// 3. Recreate verifier
	vr := verifier.NewVerifier(verifier.Config{
		DurationToleranceSec: newCfg.Verifier.DurationToleranceSec,
		MinConfidenceScore:   newCfg.Verifier.MinConfidenceScore,
		AllowPartialMatch:    newCfg.Verifier.AllowPartialMatch,
	})

	// 4. Recreate importer
	imp := importer.NewImporter(importer.Config{
		MusicPath:         newCfg.Library.MusicPath,
		FolderFormat:      newCfg.Library.FolderFormat,
		FileFormat:        newCfg.Library.FileFormat,
		DeleteAfterImport: newCfg.Import.DeleteAfterImport,
		MoveOrCopy:        newCfg.Import.MoveOrCopy,
		CleanEmptyFolders: newCfg.Import.CleanEmptyFolders,
		DownloadCoverArt:  newCfg.Export.DownloadCoverArt,
		CoverFilename:     newCfg.Export.CoverFilename,
		WriteNFO:          newCfg.Export.WriteNFO,
		GenerateM3U:       newCfg.Export.GenerateM3U,
	})

	// 5. Recreate navidrome
	var navi *navidrome.Client
	if newCfg.Navidrome.Enabled {
		navi = navidrome.NewClient(navidrome.Config{
			URL:       newCfg.Navidrome.URL,
			Username:  newCfg.Navidrome.Username,
			Password:  newCfg.Navidrome.Password,
			TokenAuth: newCfg.Navidrome.TokenAuth,
		})
	}

	s.cfg = newCfg
	s.navidrome = navi
	s.orch.UpdateConfig(newCfg, idx, dl, vr, imp, navi)
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "findr",
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	ctx := r.Context()
	artists, _ := s.repo.ListArtists(ctx)
	albums, _ := s.repo.ListMonitoredAlbums(ctx)

	completed := 0
	downloading := 0
	failed := 0
	for _, a := range albums {
		switch a.Status {
		case domain.StatusCompleted:
			completed++
		case domain.StatusDownloading, domain.StatusVerifying, domain.StatusImporting:
			downloading++
		case domain.StatusFailed:
			failed++
		}
	}

	disks := system.GetDisksUsage([]string{cfg.Library.MusicPath, cfg.Library.DownloadPath, "/"})
	maxConcurrent, activeDLs, pendingDLs := s.orch.GetQueueStatus()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                   "running",
		"uptime_seconds":           int(time.Since(s.startTime).Seconds()),
		"monitored_artists":        len(artists),
		"monitored_albums":         len(albums),
		"completed_albums":         completed,
		"downloading_albums":       downloading,
		"failed_albums":            failed,
		"max_concurrent_downloads": maxConcurrent,
		"active_downloads":         activeDLs,
		"pending_downloads":        pendingDLs,
		"music_path":               cfg.Library.MusicPath,
		"download_path":            cfg.Library.DownloadPath,
		"downloader":               cfg.Downloader.Preferred,
		"navidrome_enabled":        cfg.Navidrome.Enabled,
		"disks":                    disks,
	})
}

func (s *Server) handleDisks(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	disks := system.GetDisksUsage([]string{cfg.Library.MusicPath, cfg.Library.DownloadPath, "/"})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(disks)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]domain.Artist{})
		return
	}

	artists, err := s.orch.SearchArtists(r.Context(), query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(artists)
}

func (s *Server) handleSearchPreview(w http.ResponseWriter, r *http.Request) {
	artistID := strings.TrimSpace(r.URL.Query().Get("artist_id"))
	if artistID == "" {
		http.Error(w, "artist_id is required", http.StatusBadRequest)
		return
	}

	albums, err := s.orch.GetArtistReleasesFromIndexer(r.Context(), artistID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(albums)
}

func (s *Server) handleArtists(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		artists, err := s.repo.ListArtists(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(artists)

	case http.MethodPost:
		var req struct {
			Query       string `json:"query"`
			ID          string `json:"id"`
			SourceID    string `json:"source_id"`
			Source      string `json:"source"`
			Name        string `json:"name"`
			ImageURL    string `json:"image_url"`
			Overview    string `json:"overview"`
			Monitored   *bool  `json:"monitored"`
			DownloadAll bool   `json:"download_all"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		// Mode A: Direct artist selection from search page
		if req.SourceID != "" || req.ID != "" {
			monitored := true
			if req.Monitored != nil {
				monitored = *req.Monitored
			}
			artist := &domain.Artist{
				ID:        req.ID,
				SourceID:  req.SourceID,
				Source:    req.Source,
				Name:      req.Name,
				ImageURL:  req.ImageURL,
				Overview:  req.Overview,
				Monitored: monitored,
			}
			savedArtist, albums, err := s.orch.AddArtist(ctx, artist, monitored, req.DownloadAll)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"artist":         savedArtist,
				"indexed_albums": len(albums),
				"message":        fmt.Sprintf("Successfully added %s to library", savedArtist.Name),
			})
			return
		}

		// Mode B: Query string based search-and-add (legacy / CLI)
		if strings.TrimSpace(req.Query) == "" {
			http.Error(w, "invalid request: query or artist info required", http.StatusBadRequest)
			return
		}

		if req.DownloadAll {
			fmt.Printf("[findr Server] Starting background track and download for '%s'...\n", req.Query)
			go func() {
				bgCtx := context.Background()
				artist, _, err := s.orch.SearchAndAddArtist(bgCtx, req.Query)
				if err != nil {
					fmt.Printf("[findr Server ERROR] Track & Download failed for '%s': %v\n", req.Query, err)
					return
				}
				// Enqueue rather than download inline so releases use all configured download slots.
				count, err := s.orch.EnqueueArtistReleases(bgCtx, artist.ID, nil)
				if err != nil {
					fmt.Printf("[findr Server ERROR] Enqueue releases for '%s' failed: %v\n", artist.Name, err)
				} else {
					fmt.Printf("[findr Server] Tracking '%s': enqueued %d releases\n", artist.Name, count)
				}
			}()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"message": fmt.Sprintf("Started tracking and downloading all releases for '%s'", req.Query),
			})
			return
		}

		artist, albums, err := s.orch.SearchAndAddArtist(ctx, req.Query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"artist":         artist,
			"indexed_albums": len(albums),
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleArtistDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/api/artists/")
	parts := strings.Split(path, "/")
	artistID := parts[0]

	if artistID == "" {
		http.Error(w, "missing artist id", http.StatusBadRequest)
		return
	}

	if artistID == "download-all" {
		s.handleDownloadAllArtists(w, r)
		return
	}

	if len(parts) == 2 && parts[1] == "download-all" && r.Method == http.MethodPost {
		fmt.Printf("[findr Server] Starting discography download for artist ID %s...\n", artistID)
		go func() {
			bgCtx := context.Background()
			count, err := s.orch.EnqueueArtistReleases(bgCtx, artistID, nil)
			if err != nil {
				fmt.Printf("[findr Server ERROR] Discography download enqueue failed for artist %s: %v\n", artistID, err)
			} else {
				fmt.Printf("[findr Server SUCCESS] Enqueued %d releases for artist %s\n", count, artistID)
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": fmt.Sprintf("Triggered discography download for artist %s", artistID),
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		artist, err := s.repo.GetArtist(ctx, artistID)
		if err != nil || artist == nil {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}

		albums, _ := s.repo.ListAlbumsByArtist(ctx, artistID)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"artist": artist,
			"albums": albums,
		})

	case http.MethodDelete:
		if err := s.orch.DeleteArtist(ctx, artistID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": fmt.Sprintf("Artist %s deleted from library", artistID),
		})

	case http.MethodPut:
		var req struct {
			Monitored bool `json:"monitored"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		artist, err := s.repo.GetArtist(ctx, artistID)
		if err != nil || artist == nil {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		artist.Monitored = req.Monitored
		if err := s.repo.SaveArtist(ctx, artist); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(artist)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleDownloadAllArtists(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ReleaseTypes []string `json:"release_types"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Quick reconcile with library disk so already downloaded files are marked completed
	if r.URL.Query().Get("rescan") != "false" {
		_, _ = s.orch.ImportArtistsFromLibrary(r.Context(), nil)
	}

	var rTypes []domain.ReleaseType
	for _, rt := range req.ReleaseTypes {
		rTypes = append(rTypes, domain.ReleaseType(rt))
	}

	totalArtists, enqueued, err := s.orch.EnqueueAllMissingReleases(r.Context(), rTypes)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   err.Error(),
			"message": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "ok",
		"total_artists":  totalArtists,
		"enqueued_count": enqueued,
		"message":        fmt.Sprintf("Enqueued %d missing releases across %d monitored artists", enqueued, totalArtists),
	})
}

func (s *Server) handleAlbums(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleAddAlbum(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	albums, err := s.repo.ListMonitoredAlbums(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	statusFilter := r.URL.Query().Get("status")
	if statusFilter != "" && statusFilter != "all" {
		var filtered []domain.Album
		for _, a := range albums {
			if string(a.Status) == statusFilter {
				filtered = append(filtered, a)
			}
		}
		albums = filtered
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(albums)
}

func (s *Server) handleAlbumBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Action   string   `json:"action"` // "download", "monitor", "unmonitor"
		AlbumIDs []string `json:"album_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.AlbumIDs) == 0 {
		http.Error(w, "no album IDs provided", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	switch req.Action {
	case "download":
		count, err := s.orch.EnqueueBatchDownloads(req.AlbumIDs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"queued":  count,
			"message": fmt.Sprintf("Enqueued %d releases for download", count),
		})

	case "monitor", "unmonitor":
		monitored := req.Action == "monitor"
		updated := 0
		for _, id := range req.AlbumIDs {
			alb, err := s.repo.GetAlbum(ctx, id)
			if err == nil && alb != nil {
				alb.Monitored = monitored
				if err := s.repo.SaveAlbum(ctx, alb); err == nil {
					updated++
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"updated": updated,
			"message": fmt.Sprintf("Updated %d releases", updated),
		})

	default:
		http.Error(w, fmt.Sprintf("unknown action: %s", req.Action), http.StatusBadRequest)
	}
}

func (s *Server) handleAlbumActions(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/albums/")
	parts := strings.Split(path, "/")
	albumID := parts[0]

	if albumID == "batch" {
		s.handleAlbumBatch(w, r)
		return
	}

	if albumID == "" {
		http.Error(w, "missing album id", http.StatusBadRequest)
		return
	}

	if len(parts) == 2 && parts[1] == "download" && r.Method == http.MethodPost {
		jobID, err := s.orch.EnqueueAlbumDownload(albumID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"job_id":  jobID,
			"message": fmt.Sprintf("Download queued for album %s", albumID),
		})
		return
	}

	if len(parts) == 2 && parts[1] == "files" {
		s.handleAlbumFiles(w, r, albumID)
		return
	}

	ctx := r.Context()
	switch r.Method {
	case http.MethodDelete:
		if err := s.orch.RemoveAlbum(ctx, albumID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Release removed from findr; files on disk were kept"})

	case http.MethodGet:
		album, err := s.orch.EnsureAlbumTracks(ctx, albumID)
		if err != nil || album == nil {
			album, err = s.repo.GetAlbum(ctx, albumID)
		}
		if err != nil || album == nil {
			http.Error(w, "album not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(album)

	case http.MethodPut:
		var req struct {
			Monitored *bool `json:"monitored"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		album, err := s.repo.GetAlbum(ctx, albumID)
		if err != nil || album == nil {
			http.Error(w, "album not found", http.StatusNotFound)
			return
		}
		if req.Monitored != nil {
			album.Monitored = *req.Monitored
		}
		if err := s.repo.SaveAlbum(ctx, album); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(album)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// EnrichedDownloadJob provides full context for Web UI activity and history.
type EnrichedDownloadJob struct {
	domain.DownloadJob
	AlbumTitle  string `json:"album_title"`
	ArtistName  string `json:"artist_name"`
	CoverURL    string `json:"cover_url"`
	ReleaseDate string `json:"release_date,omitempty"`
	Year        int    `json:"year,omitempty"`
}

func (s *Server) enrichJobs(ctx context.Context, jobs []domain.DownloadJob) []EnrichedDownloadJob {
	enriched := make([]EnrichedDownloadJob, 0, len(jobs))
	for _, j := range jobs {
		ej := EnrichedDownloadJob{DownloadJob: j}
		if j.AlbumID != "" {
			album, err := s.repo.GetAlbum(ctx, j.AlbumID)
			if err == nil && album != nil {
				ej.AlbumTitle = album.Title
				ej.ArtistName = album.ArtistName
				ej.CoverURL = album.CoverURL
				ej.ReleaseDate = album.ReleaseDate
				ej.Year = album.Year

				if ej.ArtistName == "" && album.ArtistID != "" {
					art, err := s.repo.GetArtist(ctx, album.ArtistID)
					if err == nil && art != nil {
						ej.ArtistName = art.Name
					}
				}
			}
		}
		enriched = append(enriched, ej)
	}
	return enriched
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobs, err := s.orch.GetDownloadQueue(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get download queue: %v", err), http.StatusInternalServerError)
		return
	}

	enriched := s.enrichJobs(r.Context(), jobs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enriched)
}

func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	max, active, pending := s.orch.GetQueueStatus()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"max_concurrent":    max,
		"active":            active,
		"pending":           pending,
		"active_downloads":  active,
		"pending_downloads": pending,
	})
}

func (s *Server) handleQueueCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AlbumID string `json:"album_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AlbumID == "" {
		http.Error(w, "album_id is required", http.StatusBadRequest)
		return
	}

	if err := s.orch.CancelDownload(req.AlbumID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "cancelled",
		"message": fmt.Sprintf("Cancelled download for album %s", req.AlbumID),
	})
}

func (s *Server) handleImportUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(500 << 20); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse multipart form: %v", err), http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	stagingDir := filepath.Join(cfg.Library.DownloadPath, "manual_uploads", fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf("create upload directory failed: %v", err), http.StatusInternalServerError)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		http.Error(w, "no files uploaded", http.StatusBadRequest)
		return
	}

	for _, fh := range files {
		src, err := fh.Open()
		if err != nil {
			http.Error(w, fmt.Sprintf("open uploaded file %s failed: %v", fh.Filename, err), http.StatusInternalServerError)
			return
		}
		destPath := filepath.Join(stagingDir, filepath.Base(fh.Filename))
		dst, err := os.Create(destPath)
		if err != nil {
			src.Close()
			http.Error(w, fmt.Sprintf("create destination file %s failed: %v", destPath, err), http.StatusInternalServerError)
			return
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			http.Error(w, fmt.Sprintf("save file %s failed: %v", fh.Filename, err), http.StatusInternalServerError)
			return
		}
	}

	candidates, err := s.orch.ScanImportPath(r.Context(), stagingDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("scan uploaded files failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"staging_dir": stagingDir,
		"files":       candidates,
	})
}

func (s *Server) handleImportScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	candidates, err := s.orch.ScanImportPath(r.Context(), strings.TrimSpace(req.Path))
	if err != nil {
		http.Error(w, fmt.Sprintf("scan path failed: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"path":  req.Path,
		"files": candidates,
	})
}

func (s *Server) handleImportExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req orchestrator.ManualImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid import request: %v", err), http.StatusBadRequest)
		return
	}

	result, err := s.orch.ManualImport(r.Context(), req)
	if err != nil {
		http.Error(w, fmt.Sprintf("manual import failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "success",
		"success":        true,
		"album_dir":      result.AlbumDir,
		"destination":    result.AlbumDir,
		"imported_files": len(result.ImportedFiles),
		"message":        fmt.Sprintf("Successfully imported %d tracks into %s", len(result.ImportedFiles), result.AlbumDir),
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	jobs, err := s.orch.GetDownloadHistory(r.Context(), limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get download history: %v", err), http.StatusInternalServerError)
		return
	}

	enriched := s.enrichJobs(r.Context(), jobs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enriched)
}

func (s *Server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.orch.ClearDownloadHistory(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("failed to clear history: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "cleared",
		"message": "Download history cleared successfully",
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		cfg := s.cfg
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)

	case http.MethodPost, http.MethodPut:
		var newCfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, fmt.Sprintf("invalid config json: %v", err), http.StatusBadRequest)
			return
		}

		if err := config.Save(s.configPath, &newCfg); err != nil {
			http.Error(w, fmt.Sprintf("failed to save config: %v", err), http.StatusInternalServerError)
			return
		}

		if err := s.reloadComponents(&newCfg); err != nil {
			http.Error(w, fmt.Sprintf("failed to hot-reload: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "saved",
			"message": "Configuration successfully saved and reloaded",
			"config":  newCfg,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleConfigRaw(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(s.configPath)
		if err != nil {
			// If file doesn't exist yet, marshal current config
			s.mu.RLock()
			data, _ = yaml.Marshal(s.cfg)
			s.mu.RUnlock()
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(data)

	case http.MethodPost, http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil || len(data) == 0 {
			http.Error(w, "empty yaml payload", http.StatusBadRequest)
			return
		}

		var newCfg config.Config
		if err := yaml.Unmarshal(data, &newCfg); err != nil {
			http.Error(w, fmt.Sprintf("invalid yaml: %v", err), http.StatusBadRequest)
			return
		}

		if err := config.Save(s.configPath, &newCfg); err != nil {
			http.Error(w, fmt.Sprintf("failed to save yaml: %v", err), http.StatusInternalServerError)
			return
		}

		if err := s.reloadComponents(&newCfg); err != nil {
			http.Error(w, fmt.Sprintf("failed to reload components: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "saved",
			"message": "YAML configuration successfully saved and reloaded",
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleTestNavidrome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		URL       string `json:"url"`
		Username  string `json:"username"`
		Password  string `json:"password"`
		TokenAuth bool   `json:"token_auth"`
	}

	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	targetURL := req.URL
	if targetURL == "" {
		targetURL = cfg.Navidrome.URL
	}
	user := req.Username
	if user == "" {
		user = cfg.Navidrome.Username
	}
	pass := req.Password
	if pass == "" {
		pass = cfg.Navidrome.Password
	}
	tokenAuth := req.TokenAuth
	if req.URL == "" {
		tokenAuth = cfg.Navidrome.TokenAuth
	}

	if targetURL == "" {
		http.Error(w, "Navidrome URL is required", http.StatusBadRequest)
		return
	}

	testClient := navidrome.NewClient(navidrome.Config{
		URL:       targetURL,
		Username:  user,
		Password:  pass,
		TokenAuth: tokenAuth,
	})

	err := testClient.Ping(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Successfully connected to Navidrome server! (Subsonic API ping ok)",
	})
}

func (s *Server) handleTestSlskd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		URL    string `json:"url"`
		APIKey string `json:"api_key"`
	}

	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	targetURL := req.URL
	if targetURL == "" {
		targetURL = cfg.Downloader.Slskd.URL
	}
	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = cfg.Downloader.Slskd.APIKey
	}

	if targetURL == "" {
		http.Error(w, "slskd URL is required", http.StatusBadRequest)
		return
	}

	testClient := slskddl.NewDownloader(slskddl.Config{
		URL:    targetURL,
		APIKey: apiKey,
	})

	err := testClient.Ping(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Successfully connected to slskd daemon!",
	})
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	navi := s.navidrome
	s.mu.RUnlock()

	if navi == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "navidrome integration not enabled",
			"message": "navidrome integration not enabled",
		})
		return
	}

	status, err := navi.StartScan(r.Context(), false)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   err.Error(),
			"message": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleLibraryScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	summary, err := s.orch.ImportArtistsFromLibrary(r.Context(), nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   err.Error(),
			"message": err.Error(),
		})
		return
	}

	// Trigger Navidrome scan if enabled
	s.mu.RLock()
	navi := s.navidrome
	s.mu.RUnlock()
	if navi != nil {
		_, _ = navi.StartScan(r.Context(), false)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"summary": summary,
		"message": fmt.Sprintf("Imported %d new artists, reconciled %d existing, matched %d albums",
			summary.ArtistsImported, summary.ArtistsReconciled, summary.AlbumsMatched),
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Serve the SPA dashboard for root and client routes
	if r.URL.Path != "/" &&
		!strings.HasPrefix(r.URL.Path, "/search") &&
		!strings.HasPrefix(r.URL.Path, "/settings") &&
		!strings.HasPrefix(r.URL.Path, "/activity") &&
		!strings.HasPrefix(r.URL.Path, "/queue") &&
		!strings.HasPrefix(r.URL.Path, "/history") &&
		!strings.HasPrefix(r.URL.Path, "/system") &&
		!strings.HasPrefix(r.URL.Path, "/import") &&
		!strings.HasPrefix(r.URL.Path, "/artists") &&
		!strings.HasPrefix(r.URL.Path, "/artist") {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboardHTML))
}

//go:embed web/index.html
var indexHTML string

//go:embed web/app.css
var appCSS string

//go:embed web/app.js
var appJS string

// dashboardHTML is the single-page UI, assembled once at startup so the browser
// gets one self-contained document (no extra asset routes to keep in sync).
var dashboardHTML = strings.NewReplacer(
	"/*__APP_CSS__*/", appCSS,
	"//__APP_JS__", appJS,
).Replace(indexHTML)
