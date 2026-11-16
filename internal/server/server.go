package server

import (
	"context"
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

	// 2. Recreate downloader
	var dl downloader.Downloader
	switch newCfg.Downloader.Preferred {
	case "slskd":
		dl = slskddl.NewDownloader(slskddl.Config{
			URL:         newCfg.Downloader.Slskd.URL,
			APIKey:      newCfg.Downloader.Slskd.APIKey,
			DownloadDir: newCfg.Downloader.Slskd.DownloadDir,
		})
	default:
		dl = qobuzdl.NewDownloader(qobuzdl.Config{
			AppID:         newCfg.Downloader.Qobuz.AppID,
			AppSecret:     newCfg.Downloader.Qobuz.AppSecret,
			Username:      newCfg.Downloader.Qobuz.Username,
			Password:      newCfg.Downloader.Qobuz.Password,
			UserToken:     newCfg.Downloader.Qobuz.UserToken,
			TargetQuality: domain.AudioQuality(newCfg.Downloader.Qobuz.TargetQuality),
		})
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
				artist, summary, err := s.orch.TrackAndDownloadArtist(bgCtx, req.Query, nil, nil)
				if err != nil {
					fmt.Printf("[findr Server ERROR] Track & Download failed for '%s': %v\n", req.Query, err)
				} else {
					fmt.Printf("[findr Server SUCCESS] Completed tracking '%s': %d downloaded, %d failed\n",
						artist.Name, summary.Successful, summary.Failed)
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

	ctx := r.Context()
	switch r.Method {
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

const dashboardHTML = `<!DOCTYPE html>
<html lang="en" data-type="software" data-radius="soft">
<head>
  <meta charset="UTF-8">
  <title>findr - Music Collection Manager</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta name="page-type" content="software">
  <meta name="site-kind" content="software">
  <meta name="description" content="High-performance music collection manager, monitor, and automated lossless FLAC audio downloader.">

  <!-- Google Fonts: Instrument Sans & DM Mono -->
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=DM+Mono:ital,wght@0,400;0,500;1,400&family=Instrument+Sans:ital,wght@0,400..700;1,400..700&display=swap" rel="stylesheet">

  <style>
    :root {
      /* High-fidelity Studio Dark Palette */
      --p-bg: #090a0f;
      --p-surface-0: #0d1017;
      --p-surface-1: #121520;
      --p-surface-2: #171b28;
      --p-surface-3: #1f2537;
      --p-border: #1e2536;
      --p-border-hover: #2e3952;
      --p-border-focus: #3b82f6;

      --p-ink: #f1f4f9;
      --p-muted: #8b95a8;
      --p-dim: #545e75;

      --p-accent: #2563eb;
      --p-accent-hover: #1d4ed8;
      --p-accent-active: #1e40af;
      --p-accent-subtle: rgba(37, 99, 235, 0.12);

      --p-success: #10b981;
      --p-success-subtle: rgba(16, 185, 129, 0.12);
      --p-warning: #f59e0b;
      --p-warning-subtle: rgba(245, 158, 11, 0.12);
      --p-danger: #ef4444;
      --p-danger-subtle: rgba(239, 68, 68, 0.12);
      --p-cyan: #06b6d4;
      --p-cyan-subtle: rgba(6, 182, 212, 0.12);

      --font-display: "Instrument Sans", -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      --font-text: "Instrument Sans", -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      --font-mono: "DM Mono", ui-monospace, "SFMono-Regular", Menlo, Monaco, Consolas, monospace;

      --r-sm: 6px;
      --r-md: 10px;
      --r-lg: 14px;
      --ease: cubic-bezier(.2, .8, .2, 1);
      --dur: 160ms;
    }

    * { box-sizing: border-box; }
    html, body {
      margin: 0;
      padding: 0;
      background: var(--p-bg);
      color: var(--p-ink);
      font-family: var(--font-text);
      font-size: 14px;
      line-height: 1.5;
      -webkit-font-smoothing: antialiased;
      -moz-osx-font-smoothing: grayscale;
      height: 100vh;
      overflow: hidden;
    }

    /* SVG Icon Utility */
    .icon {
      display: inline-block;
      vertical-align: middle;
      flex-shrink: 0;
      stroke: currentColor;
    }

    /* Layout Shell */
    .app-shell {
      display: flex;
      height: 100vh;
      width: 100vw;
      overflow: hidden;
    }

    /* Sidebar Navigation */
    .sidebar {
      width: 256px;
      background: var(--p-surface-1);
      border-right: 1px solid var(--p-border);
      display: flex;
      flex-direction: column;
      flex-shrink: 0;
      user-select: none;
      z-index: 30;
    }

    .brand {
      padding: 22px 20px 18px;
      display: flex;
      align-items: center;
      gap: 12px;
      border-bottom: 1px solid var(--p-border);
    }
    .brand-mark {
      width: 36px;
      height: 36px;
      background: var(--p-surface-2);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--p-accent);
      box-shadow: 0 2px 8px rgba(0, 0, 0, 0.25);
    }
    .brand-info {
      display: flex;
      flex-direction: column;
      gap: 1px;
    }
    .brand-name {
      font-size: 18px;
      font-weight: 700;
      letter-spacing: -0.4px;
      color: var(--p-ink);
      line-height: 1.2;
    }
    .brand-tag {
      font-family: var(--font-mono);
      font-size: 9.5px;
      letter-spacing: 0.6px;
      color: var(--p-muted);
      text-transform: uppercase;
    }
    .brand-version {
      font-family: var(--font-mono);
      font-size: 11px;
      color: var(--p-dim);
      background: var(--p-surface-0);
      border: 1px solid var(--p-border);
      padding: 2px 6px;
      border-radius: var(--r-sm);
      margin-left: auto;
    }

    .nav-links {
      padding: 16px 12px;
      list-style: none;
      margin: 0;
      display: flex;
      flex-direction: column;
      gap: 4px;
      flex: 1;
      overflow-y: auto;
    }
    .nav-links li { margin: 0; padding: 0; }
    .nav-item {
      display: flex;
      align-items: center;
      gap: 12px;
      padding: 10px 14px;
      border-radius: var(--r-sm);
      color: var(--p-muted);
      text-decoration: none;
      font-size: 14px;
      font-weight: 500;
      cursor: pointer;
      position: relative;
      transition: color var(--dur) var(--ease), background var(--dur) var(--ease);
    }
    .nav-item:hover {
      color: var(--p-ink);
      background: rgba(255, 255, 255, 0.04);
    }
    .nav-item.active {
      color: var(--p-ink);
      background: var(--p-surface-2);
      font-weight: 600;
    }
    .nav-item.active::before {
      content: "";
      position: absolute;
      left: 0;
      top: 6px;
      bottom: 6px;
      width: 3px;
      border-radius: 0 3px 3px 0;
      background: var(--p-accent);
    }
    .nav-icon {
      color: inherit;
      display: flex;
      align-items: center;
      justify-content: center;
      width: 18px;
      height: 18px;
    }
    .nav-badge {
      font-family: var(--font-mono);
      font-size: 10.5px;
      padding: 1px 7px;
      border-radius: 999px;
      background: var(--p-surface-0);
      border: 1px solid var(--p-border);
      color: var(--p-muted);
      margin-left: auto;
    }
    .nav-badge.active-pulse {
      background: var(--p-accent-subtle);
      border-color: var(--p-accent);
      color: var(--p-accent);
      font-weight: 600;
    }

    .sidebar-footer {
      padding: 16px;
      border-top: 1px solid var(--p-border);
      display: flex;
      flex-direction: column;
      gap: 12px;
      background: var(--p-surface-0);
    }
    .btn-scan-global {
      width: 100%;
      background: var(--p-surface-2);
      border: 1px solid var(--p-border);
      color: var(--p-ink);
      padding: 10px 14px;
      border-radius: var(--r-sm);
      font-size: 13px;
      font-weight: 500;
      cursor: pointer;
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
      transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease), transform var(--dur) var(--ease);
    }
    .btn-scan-global:hover {
      background: var(--p-surface-3);
      border-color: var(--p-border-hover);
      transform: translateY(-1px);
    }
    .btn-scan-global:active {
      transform: translateY(1px);
    }
    .scan-spin {
      animation: spin 1s linear infinite;
    }
    @keyframes spin { 100% { transform: rotate(360deg); } }

    .core-status-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      font-size: 11.5px;
      color: var(--p-muted);
      padding: 0 4px;
    }
    .core-status-indicator {
      display: flex;
      align-items: center;
      gap: 6px;
    }
    .pulse-dot {
      width: 7px;
      height: 7px;
      border-radius: 50%;
      background: var(--p-success);
      box-shadow: 0 0 6px rgba(16, 185, 129, 0.6);
      animation: pulse-ring 2s infinite;
    }
    @keyframes pulse-ring {
      0% { opacity: 0.6; transform: scale(0.95); }
      50% { opacity: 1; transform: scale(1.15); }
      100% { opacity: 0.6; transform: scale(0.95); }
    }

    /* Main Content Wrapper */
    .main-wrapper {
      flex: 1;
      display: flex;
      flex-direction: column;
      height: 100vh;
      overflow-y: auto;
      background: var(--p-bg);
      position: relative;
    }

    .top-header {
      padding: 16px 32px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      border-bottom: 1px solid var(--p-border);
      background: rgba(14, 17, 26, 0.85);
      backdrop-filter: blur(14px);
      -webkit-backdrop-filter: blur(14px);
      position: sticky;
      top: 0;
      z-index: 20;
    }
    .header-left {
      display: flex;
      align-items: center;
      gap: 16px;
    }
    .header-title {
      font-size: 18px;
      font-weight: 700;
      margin: 0;
      letter-spacing: -0.3px;
      color: var(--p-ink);
    }
    .status-badge {
      font-family: var(--font-mono);
      font-size: 11.5px;
      background: var(--p-success-subtle);
      color: var(--p-success);
      border: 1px solid rgba(16, 185, 129, 0.25);
      padding: 3px 9px;
      border-radius: 999px;
      font-weight: 500;
      display: inline-flex;
      align-items: center;
      gap: 6px;
    }
    .status-dot-sm {
      width: 5px;
      height: 5px;
      background: currentColor;
      border-radius: 50%;
    }

    .page-container {
      padding: 32px;
      flex: 1;
      max-width: 1440px;
      margin: 0 auto;
      width: 100%;
    }
    .page-view {
      display: none;
    }
    .page-view.active {
      display: block;
      animation: fadeIn 0.16s var(--ease);
    }
    @keyframes fadeIn {
      from { opacity: 0; transform: translateY(4px); }
      to { opacity: 1; transform: translateY(0); }
    }

    /* Core UI Buttons */
    .btn {
      background: var(--p-accent);
      color: #ffffff;
      border: 1px solid transparent;
      border-radius: var(--r-sm);
      padding: 9px 16px;
      font-family: var(--font-text);
      font-weight: 500;
      font-size: 13.5px;
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      gap: 7px;
      transition: background var(--dur) var(--ease), transform var(--dur) var(--ease), border-color var(--dur) var(--ease);
      line-height: 1;
      user-select: none;
    }
    .btn:hover {
      background: var(--p-accent-hover);
      transform: translateY(-1px);
    }
    .btn:active {
      transform: translateY(1px);
    }
    .btn-secondary {
      background: var(--p-surface-2);
      border-color: var(--p-border);
      color: var(--p-ink);
    }
    .btn-secondary:hover {
      background: var(--p-surface-3);
      border-color: var(--p-border-hover);
    }
    .btn-success {
      background: var(--p-success);
      color: #042f1a;
      font-weight: 600;
    }
    .btn-success:hover {
      background: #059669;
      color: #ffffff;
    }
    .btn-danger {
      background: var(--p-danger-subtle);
      border-color: rgba(239, 68, 68, 0.3);
      color: #fca5a5;
    }
    .btn-danger:hover {
      background: var(--p-danger);
      color: #ffffff;
    }
    .btn-sm {
      padding: 6px 12px;
      font-size: 12.5px;
    }

    /* Input & Form Styling */
    input[type="text"], input[type="password"], input[type="number"], select, textarea {
      width: 100%;
      background: var(--p-surface-0);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      padding: 10px 13px;
      color: var(--p-ink);
      font-family: var(--font-text);
      font-size: 13.5px;
      outline: none;
      transition: border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
    }
    input:focus, select:focus, textarea:focus {
      border-color: var(--p-border-focus);
      box-shadow: 0 0 0 3px rgba(37, 99, 235, 0.15);
    }
    label {
      display: block;
      font-size: 13px;
      font-weight: 600;
      color: var(--p-ink);
      margin-bottom: 6px;
    }
    .form-group {
      margin-bottom: 20px;
    }
    .form-hint {
      font-size: 12px;
      color: var(--p-muted);
      margin-top: 5px;
      line-height: 1.4;
    }
    .code-tag {
      font-family: var(--font-mono);
      font-size: 11px;
      background: var(--p-surface-0);
      border: 1px solid var(--p-border);
      padding: 2px 6px;
      border-radius: 4px;
      color: var(--p-cyan);
    }

    /* Common Card Styles */
    .card {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-md);
      padding: 24px;
      transition: border-color var(--dur) var(--ease);
    }
    .card:hover {
      border-color: var(--p-border-hover);
    }

    /* Grids */
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
      gap: 20px;
    }
    .grid-wide {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(360px, 1fr));
      gap: 20px;
    }

    /* Artist Library Cards */
    .artist-card {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-md);
      overflow: hidden;
      display: flex;
      flex-direction: column;
      position: relative;
      transition: transform var(--dur) var(--ease), border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
    }
    .artist-card:hover {
      transform: translateY(-2px);
      border-color: var(--p-border-hover);
      box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);
    }
    .artist-cover-container {
      width: 100%;
      height: 190px;
      background: var(--p-surface-0);
      position: relative;
      overflow: hidden;
      cursor: pointer;
    }
    .artist-cover {
      width: 100%;
      height: 100%;
      object-fit: cover;
      transition: transform 0.22s var(--ease);
    }
    .artist-card:hover .artist-cover {
      transform: scale(1.03);
    }
    .artist-badge-top {
      position: absolute;
      top: 10px;
      right: 10px;
      background: rgba(9, 10, 15, 0.82);
      backdrop-filter: blur(6px);
      color: var(--p-ink);
      font-family: var(--font-mono);
      font-size: 10px;
      font-weight: 500;
      padding: 2px 7px;
      border-radius: 4px;
      border: 1px solid rgba(255, 255, 255, 0.1);
      text-transform: uppercase;
    }
    .artist-content {
      padding: 16px;
      display: flex;
      flex-direction: column;
      flex: 1;
    }
    .artist-name {
      font-size: 16px;
      font-weight: 600;
      margin: 0 0 6px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      color: var(--p-ink);
    }
    .artist-meta {
      font-family: var(--font-mono);
      font-size: 12px;
      color: var(--p-muted);
      margin-bottom: 14px;
      display: flex;
      align-items: center;
      gap: 8px;
    }

    /* Search Hero Banner */
    .search-hero {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-md);
      padding: 32px;
      margin-bottom: 28px;
    }
    .search-bar-wrap {
      display: flex;
      gap: 12px;
      margin-top: 18px;
    }
    .search-input-box {
      flex: 1;
      position: relative;
    }
    .search-input-box input {
      padding-left: 42px;
      font-size: 15px;
      height: 48px;
      border-radius: var(--r-sm);
    }
    .search-input-box .search-icon {
      position: absolute;
      left: 14px;
      top: 50%;
      transform: translateY(-50%);
      color: var(--p-muted);
      pointer-events: none;
    }
    .search-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      margin-top: 14px;
      align-items: center;
    }
    .chip {
      background: var(--p-surface-2);
      border: 1px solid var(--p-border);
      border-radius: 999px;
      padding: 4px 12px;
      font-size: 12px;
      color: var(--p-muted);
      cursor: pointer;
      transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease), color var(--dur) var(--ease);
    }
    .chip:hover {
      background: var(--p-accent-subtle);
      border-color: var(--p-accent);
      color: var(--p-ink);
    }

    /* Modal / Dialog */
    .modal-overlay {
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.8);
      backdrop-filter: blur(8px);
      -webkit-backdrop-filter: blur(8px);
      z-index: 100;
      display: none;
      align-items: center;
      justify-content: center;
      padding: 24px;
    }
    .modal-overlay.active {
      display: flex;
    }
    .modal-box {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-md);
      width: 100%;
      max-width: 860px;
      max-height: 85vh;
      display: flex;
      flex-direction: column;
      box-shadow: 0 24px 60px rgba(0, 0, 0, 0.6);
      overflow: hidden;
    }
    .modal-header {
      padding: 20px 24px;
      border-bottom: 1px solid var(--p-border);
      display: flex;
      align-items: center;
      justify-content: space-between;
      background: var(--p-surface-0);
    }
    .modal-title {
      font-size: 17px;
      font-weight: 600;
      margin: 0;
      color: var(--p-ink);
    }
    .modal-close {
      background: transparent;
      border: none;
      color: var(--p-muted);
      font-size: 20px;
      cursor: pointer;
      line-height: 1;
      padding: 4px;
      display: flex;
      align-items: center;
      justify-content: center;
      transition: color var(--dur) var(--ease);
    }
    .modal-close:hover { color: var(--p-ink); }
    .modal-body {
      padding: 24px;
      overflow-y: auto;
      flex: 1;
    }
    .modal-footer {
      padding: 16px 24px;
      border-top: 1px solid var(--p-border);
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 10px;
      background: var(--p-surface-0);
    }

    /* Tables */
    table {
      width: 100%;
      border-collapse: collapse;
      text-align: left;
    }
    th {
      font-family: var(--font-mono);
      font-size: 11px;
      text-transform: uppercase;
      letter-spacing: 0.5px;
      color: var(--p-muted);
      padding: 12px 14px;
      border-bottom: 1px solid var(--p-border);
      font-weight: 500;
    }
    td {
      padding: 12px 14px;
      border-bottom: 1px solid rgba(255, 255, 255, 0.04);
      font-size: 13.5px;
      color: var(--p-ink);
      vertical-align: middle;
    }
    tr:hover td {
      background: rgba(255, 255, 255, 0.02);
    }

    /* Settings Tabs */
    .settings-nav {
      display: flex;
      gap: 6px;
      border-bottom: 1px solid var(--p-border);
      margin-bottom: 24px;
      overflow-x: auto;
    }
    .settings-tab-btn {
      background: transparent;
      border: none;
      border-bottom: 2px solid transparent;
      color: var(--p-muted);
      padding: 10px 18px;
      font-family: var(--font-text);
      font-size: 13.5px;
      font-weight: 500;
      cursor: pointer;
      transition: color var(--dur) var(--ease), border-color var(--dur) var(--ease);
      white-space: nowrap;
    }
    .settings-tab-btn:hover {
      color: var(--p-ink);
    }
    .settings-tab-btn.active {
      color: var(--p-accent);
      border-bottom-color: var(--p-accent);
      font-weight: 600;
    }
    .settings-section {
      display: none;
    }
    .settings-section.active {
      display: block;
      animation: fadeIn 0.14s var(--ease);
    }

    /* Switch Toggle */
    .switch-label {
      display: flex;
      align-items: center;
      gap: 12px;
      cursor: pointer;
    }
    .switch-label input[type="checkbox"] {
      width: auto;
      margin: 0;
      cursor: pointer;
      accent-color: var(--p-accent);
    }

    /* Progress & Disk Bars */
    .progress-bar-container {
      width: 100%;
      height: 6px;
      background: var(--p-surface-0);
      border-radius: 4px;
      overflow: hidden;
      margin-top: 6px;
    }
    .progress-bar-fill {
      height: 100%;
      background: var(--p-accent);
      border-radius: 4px;
      transition: width 0.3s ease;
    }
    .disk-bar-container {
      width: 100%;
      height: 6px;
      background: var(--p-surface-0);
      border-radius: 4px;
      overflow: hidden;
      margin-top: 8px;
    }
    .disk-bar-fill {
      height: 100%;
      background: var(--p-success);
      border-radius: 4px;
      transition: width 0.3s ease;
    }
    .disk-bar-fill.warning { background: var(--p-warning); }
    .disk-bar-fill.danger { background: var(--p-danger); }

    /* Job Card in Queue */
    .job-card {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      padding: 14px 18px;
      margin-bottom: 10px;
      display: flex;
      align-items: center;
      gap: 16px;
      transition: border-color var(--dur) var(--ease);
    }
    .job-card:hover {
      border-color: var(--p-border-hover);
    }

    /* Error Callout */
    .error-callout {
      background: var(--p-danger-subtle);
      border: 1px solid rgba(239, 68, 68, 0.25);
      color: #fca5a5;
      border-radius: var(--r-sm);
      padding: 6px 10px;
      font-size: 11.5px;
      margin-top: 6px;
      font-family: var(--font-mono);
      word-break: break-word;
    }

    /* Floating Toast Alerts */
    .toast-container {
      position: fixed;
      bottom: 24px;
      right: 24px;
      display: flex;
      flex-direction: column;
      gap: 8px;
      z-index: 200;
      pointer-events: none;
    }
    .toast {
      background: var(--p-surface-2);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      padding: 12px 18px;
      font-size: 13.5px;
      color: var(--p-ink);
      box-shadow: 0 12px 32px rgba(0, 0, 0, 0.5);
      display: flex;
      align-items: center;
      gap: 10px;
      pointer-events: auto;
      max-width: 400px;
      animation: slideUp 0.18s var(--ease);
    }
    @keyframes slideUp {
      from { opacity: 0; transform: translateY(12px); }
      to { opacity: 1; transform: translateY(0); }
    }
    .toast.success { border-left: 3px solid var(--p-success); }
    .toast.error { border-left: 3px solid var(--p-danger); }
    .toast.info { border-left: 3px solid var(--p-accent); }

    /* Dropzone & Manual Import */
    .dropzone {
      border: 2px dashed var(--p-border);
      border-radius: var(--r-md);
      padding: 36px 20px;
      text-align: center;
      background: var(--p-surface-0);
      cursor: pointer;
      transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease);
    }
    .dropzone:hover, .dropzone.dragover {
      border-color: var(--p-accent);
      background: var(--p-surface-1);
    }
    .dropzone-icon {
      color: var(--p-accent);
      margin-bottom: 10px;
    }
    .import-mode-pane {
      display: none;
    }
    .import-mode-pane.active {
      display: block;
      animation: fadeIn 0.15s var(--ease);
    }

    /* Modal size expansion */
    .modal-box {
      max-width: 960px;
    }

    /* Discography Category Tabs & Accordion */
    .discog-tabs {
      display: flex;
      gap: 6px;
      border-bottom: 1px solid var(--p-border);
      margin-bottom: 16px;
      overflow-x: auto;
    }
    .discog-tab-btn {
      background: transparent;
      border: none;
      border-bottom: 2px solid transparent;
      color: var(--p-muted);
      padding: 8px 14px;
      font-size: 13px;
      font-weight: 500;
      cursor: pointer;
      display: flex;
      align-items: center;
      gap: 6px;
      white-space: nowrap;
      transition: color var(--dur) var(--ease), border-color var(--dur) var(--ease);
    }
    .discog-tab-btn:hover { color: var(--p-ink); }
    .discog-tab-btn.active {
      color: var(--p-accent);
      border-bottom-color: var(--p-accent);
      font-weight: 600;
    }
    .discog-counter {
      font-family: var(--font-mono);
      font-size: 11px;
      background: var(--p-surface-0);
      padding: 1px 6px;
      border-radius: 999px;
      border: 1px solid var(--p-border);
    }
    .batch-toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 8px 14px;
      background: var(--p-surface-0);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      margin-bottom: 16px;
      font-size: 13px;
      flex-wrap: wrap;
      gap: 10px;
    }
    .release-card {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      margin-bottom: 10px;
      overflow: hidden;
      transition: border-color var(--dur) var(--ease);
    }
    .release-card:hover { border-color: var(--p-border-hover); }
    .release-header {
      padding: 12px 16px;
      display: flex;
      align-items: center;
      gap: 14px;
      cursor: pointer;
      user-select: none;
    }
    .release-tracklist {
      display: none;
      border-top: 1px solid var(--p-border);
      background: var(--p-surface-0);
      padding: 12px 16px;
    }
    .release-tracklist.open {
      display: block;
      animation: fadeIn 0.15s var(--ease);
    }
    .status-pill {
      font-family: var(--font-mono);
      font-size: 11px;
      padding: 2px 7px;
      border-radius: 4px;
      text-transform: capitalize;
      font-weight: 500;
    }
    .status-completed { background: var(--p-success-subtle); color: var(--p-success); border: 1px solid rgba(16, 185, 129, 0.3); }
    .status-downloading, .status-verifying, .status-importing { background: var(--p-accent-subtle); color: var(--p-accent); border: 1px solid var(--p-accent); }
    .status-pending { background: rgba(234, 179, 8, 0.15); color: #facc15; border: 1px solid rgba(234, 179, 8, 0.3); }
    .status-failed { background: var(--p-danger-subtle); color: #fca5a5; border: 1px solid rgba(239, 68, 68, 0.3); }
    .status-missing, .status-pending-download { background: var(--p-surface-2); color: var(--p-muted); border: 1px solid var(--p-border); }

    /* Dedicated Artist Page Styles (taste-skill aesthetic) */
    .artist-nav-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 24px;
      gap: 16px;
      flex-wrap: wrap;
    }
    .artist-breadcrumbs {
      display: flex;
      align-items: center;
      gap: 8px;
      font-size: 13.5px;
    }
    .crumb-link {
      color: var(--p-muted);
      text-decoration: none;
      cursor: pointer;
      transition: color var(--dur) var(--ease);
    }
    .crumb-link:hover { color: var(--p-ink); }
    .crumb-sep { color: var(--p-dim); }
    .crumb-current { color: var(--p-ink); font-weight: 600; }

    .artist-hero-card {
      position: relative;
      overflow: hidden;
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-md);
      padding: 32px;
      margin-bottom: 28px;
      box-shadow: 0 4px 24px rgba(0, 0, 0, 0.35);
    }
    .artist-hero-backdrop {
      position: absolute;
      top: -40px;
      left: -40px;
      right: -40px;
      bottom: -40px;
      background-size: cover;
      background-position: center;
      filter: blur(60px);
      opacity: 0.18;
      pointer-events: none;
      transform: translateZ(0);
    }
    .artist-hero-inner {
      position: relative;
      z-index: 1;
      display: flex;
      gap: 28px;
      align-items: flex-start;
      flex-wrap: wrap;
    }
    .artist-hero-media {
      flex-shrink: 0;
    }
    .artist-avatar-frame {
      width: 140px;
      height: 140px;
      border-radius: var(--r-md);
      overflow: hidden;
      background: var(--p-surface-2);
      border: 1px solid var(--p-border);
      box-shadow: 0 12px 32px rgba(0, 0, 0, 0.5);
    }
    .artist-avatar-img {
      width: 100%;
      height: 100%;
      object-fit: cover;
      display: block;
      transition: transform 0.25s var(--ease);
    }
    .artist-hero-card:hover .artist-avatar-img {
      transform: scale(1.03);
    }
    .artist-hero-details {
      flex: 1;
      min-width: 280px;
      display: flex;
      flex-direction: column;
    }
    .artist-badges-row {
      display: flex;
      align-items: center;
      gap: 10px;
      margin-bottom: 8px;
      flex-wrap: wrap;
    }
    .artist-hero-name {
      font-size: 32px;
      font-weight: 700;
      letter-spacing: -0.025em;
      line-height: 1.15;
      color: var(--p-ink);
      margin: 0 0 10px 0;
    }
    .artist-hero-overview {
      color: var(--p-muted);
      font-size: 13.5px;
      line-height: 1.6;
      max-width: 820px;
      margin: 0 0 18px 0;
    }
    .artist-stats-ribbon {
      display: flex;
      align-items: center;
      gap: 12px;
      margin-bottom: 22px;
      flex-wrap: wrap;
    }
    .stat-pill {
      display: flex;
      align-items: center;
      gap: 8px;
      background: var(--p-surface-0);
      border: 1px solid var(--p-border);
      padding: 6px 14px;
      border-radius: var(--r-sm);
      font-size: 13px;
    }
    .stat-num {
      font-family: var(--font-mono);
      font-weight: 600;
      color: var(--p-ink);
      font-size: 14px;
    }
    .stat-lbl {
      color: var(--p-muted);
      font-size: 12px;
    }
    .stat-pill.stat-completed .stat-num { color: var(--p-success); }
    .stat-pill.stat-missing .stat-num { color: var(--p-warning); }
    .artist-hero-actions {
      display: flex;
      align-items: center;
      gap: 10px;
      flex-wrap: wrap;
    }

    /* Discography Toolbar Header */
    .discog-toolbar-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 16px;
      flex-wrap: wrap;
    }
    .discog-controls-right {
      display: flex;
      align-items: center;
      gap: 12px;
    }
    .discog-search-wrap {
      position: relative;
      width: 220px;
    }
    .discog-search-wrap input {
      padding-left: 34px;
      font-size: 13px;
      height: 34px;
    }
    .discog-search-wrap .icon {
      position: absolute;
      left: 10px;
      top: 50%;
      transform: translateY(-50%);
      color: var(--p-muted);
      pointer-events: none;
    }
    .view-toggle-group {
      display: flex;
      border: 1px solid var(--p-border);
      background: var(--p-surface-0);
      border-radius: var(--r-sm);
      overflow: hidden;
    }
    .btn-toggle {
      background: transparent;
      border: none;
      color: var(--p-muted);
      padding: 6px 12px;
      font-size: 12.5px;
      cursor: pointer;
      display: flex;
      align-items: center;
      gap: 6px;
      font-family: var(--font-text);
      font-weight: 500;
      transition: all var(--dur) var(--ease);
    }
    .btn-toggle:hover { color: var(--p-ink); background: var(--p-surface-2); }
    .btn-toggle.active { color: var(--p-ink); background: var(--p-surface-2); font-weight: 600; }

    /* Discography Grid */
    .discog-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(210px, 1fr));
      gap: 18px;
    }
    .album-card-grid {
      background: var(--p-surface-1);
      border: 1px solid var(--p-border);
      border-radius: var(--r-sm);
      overflow: hidden;
      display: flex;
      flex-direction: column;
      transition: transform var(--dur) var(--ease), border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
      position: relative;
    }
    .album-card-grid:hover {
      transform: translateY(-2px);
      border-color: var(--p-border-hover);
      box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);
    }
    .album-grid-cover-wrap {
      position: relative;
      width: 100%;
      padding-top: 100%;
      background: var(--p-surface-0);
      overflow: hidden;
    }
    .album-grid-cover {
      position: absolute;
      top: 0;
      left: 0;
      width: 100%;
      height: 100%;
      object-fit: cover;
      transition: transform 0.25s var(--ease);
    }
    .album-card-grid:hover .album-grid-cover {
      transform: scale(1.03);
    }
    .album-grid-badge {
      position: absolute;
      top: 8px;
      right: 8px;
      z-index: 2;
    }
    .album-grid-info {
      padding: 12px 14px;
      display: flex;
      flex-direction: column;
      flex: 1;
    }
    .album-grid-title {
      font-weight: 600;
      font-size: 13.5px;
      color: var(--p-ink);
      margin-bottom: 4px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    .album-grid-meta {
      font-family: var(--font-mono);
      font-size: 11.5px;
      color: var(--p-muted);
      margin-bottom: 12px;
    }
    .album-grid-actions {
      display: flex;
      gap: 6px;
      margin-top: auto;
    }
    .lossless-pill {
      font-family: var(--font-mono);
      font-size: 9.5px;
      background: rgba(37, 99, 235, 0.15);
      color: #60a5fa;
      border: 1px solid rgba(37, 99, 235, 0.3);
      padding: 1px 5px;
      border-radius: 4px;
      text-transform: uppercase;
      font-weight: 600;
      letter-spacing: 0.3px;
    }

    /* Responsive Queries */
    @media (max-width: 768px) {
      .app-shell { flex-direction: column; }
      .sidebar { width: 100%; height: auto; border-right: none; border-bottom: 1px solid var(--p-border); }
      .nav-links { flex-direction: row; overflow-x: auto; padding: 8px 12px; }
      .sidebar-footer { display: none; }
      .page-container { padding: 18px; }
      .search-bar-wrap { flex-direction: column; }
      .artist-hero-inner { flex-direction: column; align-items: center; text-align: center; }
      .artist-badges-row { justify-content: center; }
      .artist-stats-ribbon { justify-content: center; }
      .artist-hero-actions { justify-content: center; }
      .discog-toolbar-header { flex-direction: column; align-items: stretch; }
      .discog-controls-right { justify-content: space-between; }
      .discog-search-wrap { width: 100%; }
    }
  </style>
</head>
<body>

<div class="app-shell">

  <!-- Sidebar -->
  <aside class="sidebar">
    <div class="brand">
      <div class="brand-mark">
        <!-- Minimalist findr Soundwave Icon -->
        <svg class="icon" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round">
          <path d="M4 12v-2a4 4 0 0 1 4-4h8a4 4 0 0 1 4 4v2" />
          <path d="M4 15v1a2 2 0 0 0 2 2h1" />
          <path d="M17 18h1a2 2 0 0 0 2-2v-1" />
          <path d="M9 10v7M12 7v13M15 10v7" />
        </svg>
      </div>
      <div class="brand-info">
        <span class="brand-name">findr</span>
        <span class="brand-tag">AUDIO ENGINE</span>
      </div>
      <span class="brand-version">v0.1.0</span>
    </div>

    <ul class="nav-links">
      <li>
        <a class="nav-item active" onclick="navigateTo('artists')">
          <span class="nav-icon">
            <svg class="icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/>
            </svg>
          </span>
          <span>Library</span>
        </a>
      </li>
      <li>
        <a class="nav-item" onclick="navigateTo('search')">
          <span class="nav-icon">
            <svg class="icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="11" cy="11" r="8"/><path d="m21 21-4.34-4.34"/>
            </svg>
          </span>
          <span>Search & Add</span>
        </a>
      </li>
      <li>
        <a class="nav-item" onclick="navigateTo('activity')">
          <span class="nav-icon">
            <svg class="icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/>
            </svg>
          </span>
          <span>Queue & History</span>
          <span class="nav-badge" id="sidebarQueueCount">0</span>
        </a>
      </li>
      <li>
        <a class="nav-item" onclick="navigateTo('import')">
          <span class="nav-icon">
            <svg class="icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/>
            </svg>
          </span>
          <span>Manual Import</span>
        </a>
      </li>
      <li>
        <a class="nav-item" onclick="navigateTo('settings')">
          <span class="nav-icon">
            <svg class="icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M9.67 4.14a2.34 2.34 0 0 1 4.66 0 2.34 2.34 0 0 0 3.32 1.91 2.34 2.34 0 0 1 2.33 4.03 2.34 2.34 0 0 0 0 3.83 2.34 2.34 0 0 1-2.33 4.03 2.34 2.34 0 0 0-3.32 1.92 2.34 2.34 0 0 1-4.66 0 2.34 2.34 0 0 0-3.32-1.92 2.34 2.34 0 0 1-2.33-4.03 2.34 2.34 0 0 0 0-3.83 2.34 2.34 0 0 1 2.33-4.03 2.34 2.34 0 0 0 3.32-1.92Z"/><circle cx="12" cy="12" r="3"/>
            </svg>
          </span>
          <span>Settings</span>
        </a>
      </li>
      <li>
        <a class="nav-item" onclick="navigateTo('system')">
          <span class="nav-icon">
            <svg class="icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect width="20" height="8" x="2" y="2" rx="2"/><rect width="20" height="8" x="2" y="14" rx="2"/><path d="M6 6h.01M6 18h.01"/>
            </svg>
          </span>
          <span>System & Disks</span>
        </a>
      </li>
    </ul>

    <div class="sidebar-footer">
      <button class="btn-scan-global" id="btnSidebarScan" type="button" onclick="triggerNavidromeScan()">
        <svg class="icon" id="scanIcon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>
        </svg>
        <span>Trigger Navidrome Scan</span>
      </button>

      <div class="core-status-row">
        <div class="core-status-indicator">
          <span class="pulse-dot"></span>
          <span>Go Core Active</span>
        </div>
        <span style="font-family: var(--font-mono); font-size: 11px;">SQLite</span>
      </div>
    </div>
  </aside>

  <!-- Main View Area -->
  <div class="main-wrapper">
    <header class="top-header">
      <div class="header-left">
        <h2 class="header-title" id="pageTitle">Library</h2>
        <span class="status-badge" id="systemStatusPill">
          <span class="status-dot-sm"></span> Core Online
        </span>
      </div>
      <div style="display: flex; gap: 10px; align-items: center;">
        <button class="btn btn-secondary btn-sm" onclick="navigateTo('search')">
          <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
          Add Artist
        </button>
      </div>
    </header>

    <div class="page-container">

      <!-- PAGE 1: Artists Library -->
      <div id="view-artists" class="page-view active">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 24px;">
          <div style="display: flex; gap: 14px; align-items: center;">
            <div style="position: relative; width: 280px;">
              <input type="text" id="libraryFilter" placeholder="Filter library artists..." style="padding-left: 36px;" oninput="renderFilteredArtists()">
              <svg class="icon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" style="position: absolute; left: 12px; top: 50%; transform: translateY(-50%); color: var(--p-muted);">
                <circle cx="11" cy="11" r="8"/><path d="m21 21-4.34-4.34"/>
              </svg>
            </div>
            <span style="color: var(--p-muted); font-size: 13.5px; font-family: var(--font-mono);" id="artistCountSummary">Loading...</span>
          </div>
          <div style="display: flex; gap: 8px;">
            <button class="btn btn-primary btn-sm" id="btn_download_all_artists" onclick="downloadAllArtistsReleases()" title="Check all monitored artists and download all missing releases">
              <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>
              Download All Missing
            </button>
            <button class="btn btn-secondary btn-sm" id="btn_scan_library" onclick="scanLibraryArtists()" title="Automatically scan and import existing artists and albums from library">
              <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/></svg>
              Import From Library
            </button>
            <button class="btn btn-secondary btn-sm" onclick="loadLibraryArtists()">
              <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>
              Refresh
            </button>
          </div>
        </div>

        <div class="grid" id="libraryGrid">
          <div style="color: var(--p-muted); padding: 60px 20px; text-align: center; grid-column: 1 / -1; background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">
            Loading monitored artists...
          </div>
        </div>
      </div>

      <!-- PAGE: Dedicated Artist View -->
      <div id="view-artist" class="page-view">
        <div class="artist-nav-bar">
          <div style="display: flex; align-items: center; gap: 14px;">
            <button class="btn btn-secondary btn-sm" onclick="navigateBackFromArtist()" aria-label="Back to Library">
              <svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5"><path d="m15 18-6-6 6-6"/></svg>
              <span id="artistBackButtonLabel">Back to Library</span>
            </button>
            <div class="artist-breadcrumbs">
              <span class="crumb-link" id="artistBreadcrumbRoot" onclick="navigateBackFromArtist()">Library</span>
              <span class="crumb-sep">/</span>
              <span class="crumb-current" id="artistBreadcrumbName">Artist</span>
            </div>
          </div>
          <div style="display: flex; gap: 8px; align-items: center;" id="artistTopActions"></div>
        </div>

        <!-- Artist Studio Hero -->
        <div class="artist-hero-card" id="artistHeroCard">
          <div class="artist-hero-backdrop" id="artistHeroBackdrop"></div>
          <div class="artist-hero-inner">
            <div class="artist-hero-media">
              <div class="artist-avatar-frame">
                <img class="artist-avatar-img" id="artistHeroImg" src="" alt="">
              </div>
            </div>
            <div class="artist-hero-details">
              <div class="artist-badges-row">
                <span class="code-tag" id="artistHeroSourceTag">INDEXER</span>
                <span class="status-pill status-completed" id="artistHeroMonitoredPill">Monitored</span>
                <span id="artistHeroPreviewBadge" style="display: none; background: rgba(245, 158, 11, 0.15); color: #f59e0b; border: 1px solid rgba(245, 158, 11, 0.3); font-size: 11px; font-family: var(--font-mono); padding: 2px 7px; border-radius: 4px; font-weight: 500;">CATALOG PREVIEW</span>
              </div>
              <h1 class="artist-hero-name" id="artistHeroName">Artist</h1>
              <div class="artist-hero-overview" id="artistHeroOverview"></div>

              <div class="artist-stats-ribbon" id="artistStatsRibbon">
                <div class="stat-pill">
                  <span class="stat-num" id="artistStatTotal">0</span>
                  <span class="stat-lbl">Releases</span>
                </div>
                <div class="stat-pill stat-completed">
                  <span class="stat-num" id="artistStatDownloaded">0</span>
                  <span class="stat-lbl">Downloaded</span>
                </div>
                <div class="stat-pill stat-missing">
                  <span class="stat-num" id="artistStatMissing">0</span>
                  <span class="stat-lbl">Missing</span>
                </div>
                <div class="stat-pill">
                  <span class="stat-num" id="artistStatMonitored">0</span>
                  <span class="stat-lbl">Monitored</span>
                </div>
              </div>

              <div class="artist-hero-actions" id="artistHeroActions">
                <button class="btn btn-success" id="btnArtistDownloadAll" onclick="downloadCurrentArtistAll()">
                  <svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 15V3m9 12v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/></svg>
                  <span>Download Discography</span>
                </button>
                <button class="btn btn-secondary" id="btnArtistToggleMonitored" onclick="toggleCurrentArtistMonitored()">
                  Toggle Monitoring
                </button>
                <button class="btn btn-secondary" id="btnArtistRefresh" onclick="reloadCurrentArtistPage()" title="Refresh discography from database">
                  <svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>
                  <span>Refresh</span>
                </button>
                <button class="btn btn-danger btn-sm" id="btnArtistDelete" onclick="deleteCurrentArtist()" title="Remove artist and releases from library">
                  <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
                  <span>Delete Artist</span>
                </button>
              </div>
            </div>
          </div>
        </div>

        <!-- Discography Section -->
        <div class="artist-discog-section">
          <div class="discog-toolbar-header">
            <div class="discog-tabs" id="artistCategoryTabs"></div>
            <div class="discog-controls-right">
              <div class="discog-search-wrap">
                <svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.34-4.34"/></svg>
                <input type="text" id="artistReleaseFilterInput" placeholder="Filter releases..." oninput="onArtistReleaseFilter(this.value)">
              </div>
              <div class="view-toggle-group">
                <button class="btn-toggle active" id="btnToggleList" onclick="setArtistViewMode('list')" title="Detailed List View with Tracklists">
                  <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/></svg>
                  List
                </button>
                <button class="btn-toggle" id="btnToggleGrid" onclick="setArtistViewMode('grid')" title="Album Art Grid View">
                  <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><rect width="7" height="7" x="3" y="3" rx="1"/><rect width="7" height="7" x="14" y="3" rx="1"/><rect width="7" height="7" x="14" y="14" rx="1"/><rect width="7" height="7" x="3" y="14" rx="1"/></svg>
                  Grid
                </button>
              </div>
            </div>
          </div>

          <!-- Batch Toolbar -->
          <div class="batch-toolbar" id="artistBatchToolbar"></div>

          <!-- Releases List / Grid -->
          <div id="artistReleasesList" class="artist-releases-wrap"></div>
        </div>
      </div>

      <!-- PAGE 2: Search & Add Artists -->
      <div id="view-search" class="page-view">
        <div class="search-hero">
          <h2 style="margin: 0 0 6px; font-size: 20px; font-weight: 700; color: var(--p-ink);">Search Music Catalogs</h2>
          <div style="color: var(--p-muted); font-size: 13.5px;">
            Instant metadata querying via Deezer (Zero-Auth) and lossless 24-bit/192kHz audio indexing via Qobuz. Preview discographies before tracking.
          </div>

          <div class="search-bar-wrap">
            <div class="search-input-box">
              <svg class="icon search-icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2">
                <circle cx="11" cy="11" r="8"/><path d="m21 21-4.34-4.34"/>
              </svg>
              <input type="text" id="searchQueryInput" placeholder="Search artist name (e.g. Daft Punk, Justice, Pink Floyd, Radiohead)..." onkeydown="if(event.key==='Enter') executeArtistSearch()">
            </div>
            <select id="searchProviderSelect" style="width: 180px; height: 48px;">
              <option value="deezer">Deezer (Zero-Auth)</option>
              <option value="qobuz">Qobuz Catalog</option>
            </select>
            <button class="btn" style="height: 48px; padding: 0 24px;" onclick="executeArtistSearch()">
              Search Catalog
            </button>
          </div>

          <div class="search-chips">
            <span style="font-size: 12px; color: var(--p-muted); margin-right: 4px; font-family: var(--font-mono);">Suggestions:</span>
            <span class="chip" onclick="quickSearch('Daft Punk')">Daft Punk</span>
            <span class="chip" onclick="quickSearch('Justice')">Justice</span>
            <span class="chip" onclick="quickSearch('Pink Floyd')">Pink Floyd</span>
            <span class="chip" onclick="quickSearch('Radiohead')">Radiohead</span>
            <span class="chip" onclick="quickSearch('Miles Davis')">Miles Davis</span>
            <span class="chip" onclick="quickSearch('Aphex Twin')">Aphex Twin</span>
          </div>
        </div>

        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px;">
          <h3 style="margin: 0; font-size: 15px; font-weight: 600; color: var(--p-ink);" id="searchResultsHeader">Discovered Artists</h3>
          <span style="color: var(--p-muted); font-size: 13px; font-family: var(--font-mono);" id="searchResultsCount"></span>
        </div>

        <div class="grid" id="searchResultsGrid">
          <div style="grid-column: 1 / -1; padding: 60px 20px; text-align: center; color: var(--p-muted); background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">
            Type an artist name above and search to preview and add music.
          </div>
        </div>
      </div>

      <!-- PAGE 3: Queue & Download History -->
      <div id="view-activity" class="page-view">
        <!-- Live Downloads Card -->
        <div class="card" style="margin-bottom: 28px;">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; flex-wrap: wrap; gap: 10px;">
            <div style="display: flex; align-items: center; gap: 8px;">
              <span class="pulse-dot"></span>
              <h3 style="margin: 0; font-size: 16px; font-weight: 600;">Active Pipeline & Queue</h3>
            </div>
            <div style="display: flex; align-items: center; gap: 10px;">
              <span style="font-size: 12.5px; color: var(--p-muted); font-family: var(--font-mono);">
                Limit: <strong id="queueConcurrencyDisplay" style="color: var(--p-cyan);">2</strong> simultaneous downloads
              </span>
              <span class="status-badge" id="queueBadge" style="background: var(--p-surface-0); border-color: var(--p-border); color: var(--p-muted);">
                0 Active
              </span>
            </div>
          </div>
          <div id="liveQueueContainer">
            <div style="background: var(--p-surface-0); border: 1px dashed var(--p-border); border-radius: var(--r-sm); padding: 24px; text-align: center; color: var(--p-muted); font-size: 13px;">
              Download pipeline is idle. No transfers in progress.
            </div>
          </div>
        </div>

        <!-- Download History Table Card -->
        <div class="card" id="historyTableCard">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px; flex-wrap: wrap; gap: 12px;">
            <div>
              <h3 style="margin: 0 0 4px; font-size: 16px; font-weight: 600;">Download History</h3>
              <div style="font-size: 12.5px; color: var(--p-muted);" id="historyCount">Loading history...</div>
            </div>
            <div style="display: flex; gap: 8px; align-items: center;">
              <div style="display: flex; background: var(--p-surface-0); border: 1px solid var(--p-border); border-radius: var(--r-sm); padding: 2px;">
                <button class="btn btn-secondary btn-sm active" style="border: none; background: var(--p-surface-2);" onclick="filterHistory('all', this)">All</button>
                <button class="btn btn-secondary btn-sm" style="border: none; background: transparent;" onclick="filterHistory('completed', this)">Completed</button>
                <button class="btn btn-secondary btn-sm" style="border: none; background: transparent;" onclick="filterHistory('failed', this)">Failed</button>
                <button class="btn btn-secondary btn-sm" style="border: none; background: transparent;" onclick="filterHistory('releases', this)">Releases</button>
              </div>
              <button class="btn btn-danger btn-sm" onclick="clearDownloadHistory()">
                <svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
                Clear History
              </button>
            </div>
          </div>

          <div id="historyTableContainer" style="overflow-x: auto;">
            Loading download history...
          </div>
        </div>

        <!-- Indexed Releases Card -->
        <div class="card" id="releasesTableCard" style="display: none;">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px;">
            <div>
              <h3 style="margin: 0 0 4px; font-size: 16px; font-weight: 600;">Indexed Releases</h3>
              <div style="font-size: 12.5px; color: var(--p-muted);">Albums, EPs and singles cataloged in library database</div>
            </div>
            <div style="display: flex; gap: 8px;">
              <button class="btn btn-secondary btn-sm active" onclick="filterActivity('all', this)">All</button>
              <button class="btn btn-secondary btn-sm" onclick="filterActivity('completed', this)">Downloaded</button>
              <button class="btn btn-secondary btn-sm" onclick="filterActivity('missing', this)">Missing</button>
            </div>
          </div>
          <div style="overflow-x: auto;">
            <table>
              <thead>
                <tr>
                  <th style="width: 44px;"></th>
                  <th>Title</th>
                  <th>Artist</th>
                  <th>Type</th>
                  <th>Year</th>
                  <th>Status</th>
                  <th style="text-align: right;">Action</th>
                </tr>
              </thead>
              <tbody id="activityTableBody">
                <tr><td colspan="7" style="text-align: center; padding: 30px; color: var(--p-muted);">Loading releases...</td></tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <!-- PAGE: Manual Import & Upload -->
      <div id="view-import" class="page-view">
        <div style="margin-bottom: 24px;">
          <h2 style="margin: 0 0 6px; font-size: 20px; font-weight: 700; color: var(--p-ink);">Manual Music Import & Upload</h2>
          <div style="color: var(--p-muted); font-size: 13.5px;">
            Upload local audio files directly from your browser or scan any folder on your server to match, organize, and import into your canonical music library.
          </div>
        </div>

        <div style="display: flex; gap: 8px; margin-bottom: 20px;">
          <button class="btn btn-secondary btn-sm import-mode-btn active" id="btnModeUpload" onclick="switchImportMode('upload')">
            <svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/></svg>
            Browser File Upload
          </button>
          <button class="btn btn-secondary btn-sm import-mode-btn" id="btnModeScan" onclick="switchImportMode('scan')">
            <svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
            Scan Server Directory
          </button>
        </div>

        <!-- Mode A: Upload -->
        <div id="paneUpload" class="import-mode-pane active">
          <div class="card" style="margin-bottom: 24px;">
            <div class="dropzone" id="audioDropzone" onclick="document.getElementById('audioFileInput').click()" ondragover="handleDragOver(event)" ondragleave="handleDragLeave(event)" ondrop="handleFileDrop(event)">
              <input type="file" id="audioFileInput" multiple accept=".flac,.mp3,.m4a,.wav,.ogg,.opus,.aiff" style="display: none;" onchange="handleAudioFileSelect(event)">
              <div class="dropzone-icon">
                <svg class="icon" viewBox="0 0 24 24" width="40" height="40" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/></svg>
              </div>
              <div style="font-weight: 600; font-size: 15px; margin-bottom: 4px;">Click to select audio files or drag and drop here</div>
              <div style="color: var(--p-muted); font-size: 13px;">Accepts FLAC, MP3, M4A, WAV, OGG, OPUS, and AIFF audio files</div>
            </div>

            <div id="uploadProgressContainer" style="display: none; margin-top: 16px;">
              <div style="display: flex; justify-content: space-between; font-size: 13px; font-family: var(--font-mono); margin-bottom: 6px;">
                <span id="uploadStatusText">Uploading files...</span>
                <span id="uploadPercentText" style="color: var(--p-accent);">0%</span>
              </div>
              <div class="progress-bar-container"><div class="progress-bar-fill" id="uploadProgressBar" style="width: 0%;"></div></div>
            </div>
          </div>
        </div>

        <!-- Mode B: Scan -->
        <div id="paneScan" class="import-mode-pane">
          <div class="card" style="margin-bottom: 24px;">
            <h3 style="margin: 0 0 8px; font-size: 16px; font-weight: 600;">Scan Server Directory</h3>
            <div style="color: var(--p-muted); font-size: 13px; margin-bottom: 16px;">
              Provide an absolute path on the host system to scan for unindexed audio files.
            </div>
            <div style="display: flex; gap: 10px;">
              <input type="text" id="importScanPathInput" placeholder="/downloads/incoming or /media/new_music" style="flex: 1;">
              <button class="btn" onclick="executeDirectoryScan()">Scan Directory</button>
            </div>
          </div>
        </div>

        <!-- Review & Destination Assignment Card -->
        <div class="card" id="importReviewCard" style="display: none; margin-bottom: 24px;">
          <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Detected Audio Files & Tags</h3>
          <div id="detectedFilesTableContainer" style="overflow-x: auto; margin-bottom: 24px;"></div>

          <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Target Library Destination</h3>
          <div class="grid-wide" style="margin-bottom: 16px;">
            <div class="form-group">
              <label for="importArtistSelect">Artist</label>
              <select id="importArtistSelect" onchange="handleImportArtistChange()"></select>
              <input type="text" id="importNewArtistInput" placeholder="New Artist Name" style="display: none; margin-top: 8px;">
            </div>
            <div class="form-group">
              <label for="importAlbumSelect">Album / Release</label>
              <select id="importAlbumSelect" onchange="handleImportAlbumChange()"></select>
              <input type="text" id="importNewAlbumInput" placeholder="New Album Title" style="display: none; margin-top: 8px;">
            </div>
          </div>

          <div class="grid-wide" style="margin-bottom: 20px;">
            <div class="form-group">
              <label for="importReleaseType">Release Type</label>
              <select id="importReleaseType">
                <option value="album">Album</option>
                <option value="ep">EP</option>
                <option value="single">Single</option>
                <option value="compilation">Compilation</option>
                <option value="other">Other</option>
              </select>
            </div>
            <div class="form-group">
              <label for="importYearInput">Release Year</label>
              <input type="number" id="importYearInput" value="2024">
            </div>
            <div class="form-group">
              <label for="importMoveOrCopy">File Placement</label>
              <select id="importMoveOrCopy">
                <option value="move">Atomic Move (removes from upload/staging)</option>
                <option value="copy">Copy (keeps source files)</option>
              </select>
            </div>
          </div>

          <button class="btn btn-success" style="padding: 10px 24px;" onclick="executeManualImport()">
            <svg class="icon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M20 6 9 17l-5-5"/></svg>
            Import to Music Library
          </button>
        </div>
      </div>

      <!-- PAGE 4: Settings -->
      <div id="view-settings" class="page-view">
        <div class="settings-nav">
          <button class="settings-tab-btn active" onclick="switchSettingsTab('storage', this)">Library & Storage</button>
          <button class="settings-tab-btn" onclick="switchSettingsTab('indexer', this)">Indexer & APIs</button>
          <button class="settings-tab-btn" onclick="switchSettingsTab('downloaders', this)">Downloaders</button>
          <button class="settings-tab-btn" onclick="switchSettingsTab('navidrome', this)">Navidrome Server</button>
          <button class="settings-tab-btn" onclick="switchSettingsTab('yaml', this)">Raw YAML</button>
        </div>

        <form id="settingsForm" onsubmit="saveConfiguration(event)">

          <!-- TAB 1: Library & Storage -->
          <div id="tab-storage" class="settings-section active">
            <div class="card" style="margin-bottom: 20px;">
              <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Directory Paths & Templates</h3>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_music_path">Music Library Path</label>
                  <input type="text" id="cfg_music_path" placeholder="/music">
                  <div class="form-hint">Canonical library root where organized albums and FLAC files are imported.</div>
                </div>
                <div class="form-group">
                  <label for="cfg_download_path">Downloads Staging Path</label>
                  <input type="text" id="cfg_download_path" placeholder="/downloads">
                  <div class="form-hint">Temporary directory where raw downloads from Qobuz and Soulseek land.</div>
                </div>
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_folder_format">Folder Format Template</label>
                  <input type="text" id="cfg_folder_format" placeholder="{Artist}/{Album} ({Year})">
                  <div class="form-hint">Variables: <span class="code-tag">{Artist}</span> <span class="code-tag">{Album}</span> <span class="code-tag">{Year}</span></div>
                </div>
                <div class="form-group">
                  <label for="cfg_file_format">File Format Template</label>
                  <input type="text" id="cfg_file_format" placeholder="{Track:02d} - {Title}">
                  <div class="form-hint">Variables: <span class="code-tag">{Track:02d}</span> <span class="code-tag">{Title}</span> <span class="code-tag">{Artist}</span></div>
                </div>
              </div>
            </div>

            <div class="card" style="margin-bottom: 20px;">
              <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Verification & Import Safety</h3>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_duration_tol">Duration Tolerance (Seconds)</label>
                  <input type="number" id="cfg_duration_tol" min="0" max="30" value="4">
                  <div class="form-hint">Acceptable difference between downloaded track duration and streaming catalog metadata.</div>
                </div>
                <div class="form-group">
                  <label for="cfg_min_score">Minimum Confidence Score (0.0 - 1.0)</label>
                  <input type="number" id="cfg_min_score" step="0.05" min="0.5" max="1.0" value="0.85">
                  <div class="form-hint">Fuzzy string similarity threshold required between file tags and indexer track titles.</div>
                </div>
              </div>
              <div style="display: flex; flex-direction: column; gap: 12px; margin-top: 8px;">
                <label class="switch-label">
                  <input type="checkbox" id="cfg_allow_partial">
                  <span>Allow partial album import when non-critical bonus tracks are absent</span>
                </label>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_delete_after_import" checked>
                  <span>Delete temporary download staging files after successful import</span>
                </label>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_clean_empty_folders" checked>
                  <span>Automatically clean empty staging folders</span>
                </label>
              </div>
            </div>

            <div class="card">
              <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Cover Art & Metadata Exports</h3>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_cover_filename">Cover Artwork Filename</label>
                  <input type="text" id="cfg_cover_filename" value="cover.jpg">
                  <div class="form-hint">Standard cover image name saved alongside album tracks.</div>
                </div>
                <div class="form-group">
                  <label for="cfg_move_or_copy">File Placement Operation</label>
                  <select id="cfg_move_or_copy">
                    <option value="move">Atomic Move (Fastest, zero disk duplication)</option>
                    <option value="copy">Copy (Keeps original in staging)</option>
                  </select>
                </div>
              </div>
              <div style="display: flex; gap: 24px; margin-top: 8px;">
                <label class="switch-label">
                  <input type="checkbox" id="cfg_download_cover_art" checked>
                  <span>Download full-resolution cover art (1000x1000)</span>
                </label>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_write_nfo">
                  <span>Export album.nfo metadata file</span>
                </label>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_generate_m3u">
                  <span>Generate M3U playlist file</span>
                </label>
              </div>
            </div>
          </div>

          <!-- TAB 2: Indexer & APIs -->
          <div id="tab-indexer" class="settings-section">
            <div class="card" style="margin-bottom: 20px;">
              <h3 style="margin: 0 0 8px; font-size: 16px; font-weight: 600;">Catalog Indexer Selection</h3>
              <div style="color: var(--p-muted); font-size: 13px; margin-bottom: 16px;">
                <strong>findr</strong> bypasses MusicBrainz rate limits by querying modern streaming service catalogs directly.
              </div>
              <div class="form-group" style="max-width: 400px;">
                <label for="cfg_indexer_provider">Default Metadata Indexer</label>
                <select id="cfg_indexer_provider">
                  <option value="deezer">Deezer (Zero-Auth, Immediate Search & Discography)</option>
                  <option value="qobuz">Qobuz Catalog (Direct 1:1 fidelity for downloads)</option>
                  <option value="spotify">Spotify Web API</option>
                </select>
              </div>
            </div>

            <div class="card">
              <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Spotify Developer Credentials (Optional)</h3>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_spotify_client_id">Spotify Client ID</label>
                  <input type="text" id="cfg_spotify_client_id" placeholder="Client ID from developer.spotify.com">
                </div>
                <div class="form-group">
                  <label for="cfg_spotify_client_secret">Spotify Client Secret</label>
                  <input type="password" id="cfg_spotify_client_secret" placeholder="Client Secret">
                </div>
              </div>
            </div>
          </div>

          <!-- TAB 3: Downloaders -->
          <div id="tab-downloaders" class="settings-section">
            <div class="card" style="margin-bottom: 20px;">
              <h3 style="margin: 0 0 8px; font-size: 16px; font-weight: 600;">Queue Concurrency Limit</h3>
              <div style="color: var(--p-muted); font-size: 13px; margin-bottom: 16px;">
                Configure how many releases findr downloads simultaneously. Remaining jobs wait in the pending queue and start automatically as slots open.
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_preferred_downloader">Preferred Downloader Engine</label>
                  <select id="cfg_preferred_downloader">
                    <option value="slskd">Soulseek (slskd) P2P</option>
                    <option value="qobuz">Qobuz Lossless / Hi-Res</option>
                  </select>
                  <div class="form-hint">Default engine used when adding releases or downloading albums.</div>
                </div>
                <div class="form-group">
                  <label for="cfg_max_concurrent_downloads">Max Concurrent Downloads</label>
                  <input type="number" id="cfg_max_concurrent_downloads" min="1" max="10" value="2">
                  <div class="form-hint">Default: 2. Range: 1 to 10 simultaneous downloads.</div>
                </div>
              </div>
            </div>
            <div class="card" style="margin-bottom: 20px;">
              <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 14px;">
                <h3 style="margin: 0; font-size: 16px; font-weight: 600;">Qobuz Lossless Downloader</h3>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_qobuz_dl_enabled" checked>
                  <span style="font-weight: 600;">Enabled</span>
                </label>
              </div>
              <div style="color: var(--p-muted); font-size: 13px; margin-bottom: 18px;">
                Direct lossless FLAC (16-bit/44.1kHz) and Hi-Res audio (24-bit up to 192kHz) stream downloader.
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_qobuz_target_quality">Target Audio Quality</label>
                  <select id="cfg_qobuz_target_quality">
                    <option value="flac_16_44">Lossless CD Quality (16-bit / 44.1 kHz FLAC)</option>
                    <option value="flac_24_96">Hi-Res Audio (24-bit / up to 96 kHz FLAC)</option>
                    <option value="flac_24_192">Maximum Hi-Res Studio Master (24-bit / 192 kHz FLAC)</option>
                  </select>
                </div>
                <div class="form-group">
                  <label for="cfg_qobuz_user_token">User Auth Token</label>
                  <input type="password" id="cfg_qobuz_user_token" placeholder="User auth token (e.g. from qobuz-dl)">
                  <div class="form-hint">Can also be supplied via FINDR_QOBUZ_USER_TOKEN environment variable.</div>
                </div>
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_qobuz_app_id">Qobuz App ID</label>
                  <input type="text" id="cfg_qobuz_app_id" value="798273057">
                </div>
                <div class="form-group">
                  <label for="cfg_qobuz_app_secret">Qobuz App Secret (Optional)</label>
                  <input type="password" id="cfg_qobuz_app_secret" placeholder="Optional app secret">
                </div>
              </div>
            </div>

            <div class="card">
              <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 14px;">
                <h3 style="margin: 0; font-size: 16px; font-weight: 600;">Soulseek (slskd) P2P Downloader</h3>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_slskd_enabled">
                  <span style="font-weight: 600;">Enabled</span>
                </label>
              </div>
              <div style="color: var(--p-muted); font-size: 13px; margin-bottom: 18px;">
                P2P fallback for rare vinyl rips, underground electronic releases, and out-of-print albums.
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_slskd_url">slskd Server URL</label>
                  <input type="text" id="cfg_slskd_url" placeholder="http://localhost:5030">
                </div>
                <div class="form-group">
                  <label for="cfg_slskd_api_key">slskd API Key</label>
                  <input type="password" id="cfg_slskd_api_key" placeholder="API key configured in slskd.yml">
                </div>
              </div>
              <div class="form-group">
                <label for="cfg_slskd_dir">slskd Download Directory</label>
                <input type="text" id="cfg_slskd_dir" placeholder="/downloads/slskd">
              </div>
              <div style="margin-top: 14px; display: flex; align-items: center; gap: 12px;">
                <button type="button" class="btn btn-secondary" onclick="testSlskdConnection()">Test slskd Connection</button>
                <span id="slskdTestResult" style="font-size: 13px;"></span>
              </div>
            </div>
          </div>

          <!-- TAB 4: Navidrome -->
          <div id="tab-navidrome" class="settings-section">
            <div class="card">
              <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 14px;">
                <h3 style="margin: 0; font-size: 16px; font-weight: 600;">Navidrome Subsonic Integration</h3>
                <label class="switch-label">
                  <input type="checkbox" id="cfg_navi_enabled">
                  <span style="font-weight: 600;">Enabled</span>
                </label>
              </div>
              <div style="color: var(--p-muted); font-size: 13px; margin-bottom: 18px;">
                Automatically instructs Navidrome to scan the music library via Subsonic API immediately after new albums are imported.
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_navi_url">Navidrome Server URL</label>
                  <input type="text" id="cfg_navi_url" placeholder="http://localhost:4533">
                </div>
                <div class="form-group">
                  <label for="cfg_navi_username">Subsonic Username</label>
                  <input type="text" id="cfg_navi_username" placeholder="admin">
                </div>
              </div>
              <div class="grid-wide">
                <div class="form-group">
                  <label for="cfg_navi_password">Subsonic Password</label>
                  <input type="password" id="cfg_navi_password" placeholder="Password">
                </div>
                <div class="form-group" style="display: flex; align-items: flex-end;">
                  <button type="button" class="btn btn-secondary" style="height: 42px; width: 100%;" onclick="testNavidromeConnection()">
                    <svg class="icon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/></svg>
                    Test Server Connection
                  </button>
                </div>
              </div>
              <div style="margin-top: 8px;">
                <label class="switch-label">
                  <input type="checkbox" id="cfg_navi_token_auth" checked>
                  <span>Use Subsonic token authentication (MD5 salt + token, avoids plain-text password transmission)</span>
                </label>
              </div>
              <div id="navidromeTestResult" style="margin-top: 14px; font-size: 13px; font-family: var(--font-mono);"></div>
            </div>
          </div>

          <!-- TAB 5: Raw YAML -->
          <div id="tab-yaml" class="settings-section">
            <div class="card">
              <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 14px;">
                <h3 style="margin: 0; font-size: 16px; font-weight: 600;">Raw YAML Configuration</h3>
                <div style="display: flex; gap: 8px;">
                  <input type="file" id="yamlFileInput" accept=".yaml,.yml" style="display: none;" onchange="handleYamlFileUpload(event)">
                  <button type="button" class="btn btn-secondary btn-sm" onclick="document.getElementById('yamlFileInput').click()">Upload File</button>
                  <button type="button" class="btn btn-secondary btn-sm" onclick="exportConfigFile()">Export File</button>
                  <button type="button" class="btn btn-sm" onclick="saveRawYaml()">Apply & Save YAML</button>
                </div>
              </div>
              <textarea id="rawYamlEditor" rows="22" style="font-family: var(--font-mono); font-size: 12.5px; line-height: 1.6; background: var(--p-surface-0); resize: vertical;"></textarea>
            </div>
          </div>

          <!-- Sticky Save Bar -->
          <div style="margin-top: 24px; padding: 16px 24px; background: var(--p-surface-1); border: 1px solid var(--p-border); border-radius: var(--r-md); display: flex; justify-content: space-between; align-items: center; position: sticky; bottom: 16px; z-index: 10; box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);">
            <div style="font-size: 13px; color: var(--p-muted);">
              Changes will be written to <code style="font-family: var(--font-mono); color: var(--p-cyan);">config.yaml</code> and live-reloaded in findr core memory.
            </div>
            <button type="submit" class="btn btn-success" style="padding: 10px 22px;">
              <svg class="icon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M20 6 9 17l-5-5"/></svg>
              Save & Apply Settings
            </button>
          </div>
        </form>
      </div>

      <!-- PAGE 5: System & Disks -->
      <div id="view-system" class="page-view">
        <div class="grid-wide" style="margin-bottom: 24px;">
          <div class="card">
            <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">System Runtime</h3>
            <table>
              <tr>
                <td style="color: var(--p-muted); width: 140px;">Core Engine:</td>
                <td><strong style="font-family: var(--font-mono);">findr v0.1.0 (Go core)</strong></td>
              </tr>
              <tr>
                <td style="color: var(--p-muted);">Uptime:</td>
                <td id="sysUptime" style="font-family: var(--font-mono);">-</td>
              </tr>
              <tr>
                <td style="color: var(--p-muted);">Database:</td>
                <td style="font-family: var(--font-mono);">SQLite (pure Go modernc)</td>
              </tr>
              <tr>
                <td style="color: var(--p-muted);">Monitored Artists:</td>
                <td id="sysArtistCount" style="font-family: var(--font-mono); font-weight: 600;">-</td>
              </tr>
              <tr>
                <td style="color: var(--p-muted);">Monitored Albums:</td>
                <td id="sysAlbumCount" style="font-family: var(--font-mono); font-weight: 600;">-</td>
              </tr>
            </table>
          </div>

          <div class="card">
            <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Service Integration</h3>
            <table>
              <tr>
                <td style="color: var(--p-muted); width: 140px;">Metadata Indexer:</td>
                <td id="sysIndexer" style="font-family: var(--font-mono); text-transform: uppercase;">-</td>
              </tr>
              <tr>
                <td style="color: var(--p-muted);">Downloader:</td>
                <td id="sysDownloader" style="font-family: var(--font-mono); text-transform: uppercase;">-</td>
              </tr>
              <tr>
                <td style="color: var(--p-muted);">Navidrome Scan:</td>
                <td id="sysNavidrome">-</td>
              </tr>
            </table>
          </div>
        </div>

        <div class="card">
          <h3 style="margin: 0 0 16px; font-size: 16px; font-weight: 600;">Storage Disks & Partitions</h3>
          <div id="systemDisksList">Loading storage partitions...</div>
        </div>
      </div>

    </div>
  </div>

</div>

<!-- Modal: Artist Discography & Preview -->
<div class="modal-overlay" id="artistDetailModal">
  <div class="modal-box">
    <div class="modal-header">
      <h3 class="modal-title" id="modalArtistTitle">Artist Discography</h3>
      <button class="modal-close" onclick="closeModal('artistDetailModal')">&times;</button>
    </div>
    <div class="modal-body" id="modalArtistBody">
      Loading discography...
    </div>
    <div class="modal-footer" id="modalArtistFooter">
      <button class="btn btn-secondary" onclick="closeModal('artistDetailModal')">Close</button>
    </div>
  </div>
</div>

<!-- Floating Toast Container -->
<div class="toast-container" id="toastContainer"></div>

<script>
  let currentConfig = null;
  let allArtists = [];
  let currentView = 'artists';

  function showToast(message, type = 'info') {
    const container = document.getElementById('toastContainer');
    const toast = document.createElement('div');
    toast.className = 'toast ' + type;

    let iconSvg = '';
    if (type === 'success') {
      iconSvg = '<svg class="icon" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="var(--p-success)" stroke-width="2.5"><path d="M20 6 9 17l-5-5"/></svg>';
    } else if (type === 'error') {
      iconSvg = '<svg class="icon" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="var(--p-danger)" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>';
    } else {
      iconSvg = '<svg class="icon" viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="var(--p-accent)" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></svg>';
    }

    toast.innerHTML = iconSvg + '<span>' + message + '</span>';
    container.appendChild(toast);
    setTimeout(() => {
      toast.style.opacity = '0';
      toast.style.transition = 'opacity 0.25s ease';
      setTimeout(() => toast.remove(), 250);
    }, 4000);
  }

  function navigateTo(viewName, pushHash = true) {
    currentView = viewName;
    document.querySelectorAll('.page-view').forEach(el => el.classList.remove('active'));
    document.querySelectorAll('.nav-item').forEach(el => el.classList.remove('active'));

    const targetView = document.getElementById('view-' + viewName);
    if (targetView) targetView.classList.add('active');

    const titles = {
      'artists': 'Library',
      'artist': 'Artist',
      'search': 'Search & Add Artists',
      'activity': 'Queue & History',
      'import': 'Manual Import & Upload',
      'settings': 'Settings',
      'system': 'System & Disks'
    };
    document.getElementById('pageTitle').innerText = titles[viewName] || 'findr';

    const links = document.querySelectorAll('.nav-links .nav-item');
    if ((viewName === 'artists' || viewName === 'artist') && links[0]) links[0].classList.add('active');
    if (viewName === 'search' && links[1]) links[1].classList.add('active');
    if (viewName === 'activity' && links[2]) links[2].classList.add('active');
    if (viewName === 'import' && links[3]) links[3].classList.add('active');
    if (viewName === 'settings' && links[4]) links[4].classList.add('active');
    if (viewName === 'system' && links[5]) links[5].classList.add('active');

    if (pushHash && viewName !== 'artist') {
      window.location.hash = '#' + viewName;
    }

    if (viewName === 'artists') loadLibraryArtists();
    if (viewName === 'activity') {
      loadLiveQueue();
      loadDownloadHistory();
    }
    if (viewName === 'import') initManualImport();
    if (viewName === 'settings') loadSettings();
    if (viewName === 'system') loadSystemOverview();
  }

  function switchSettingsTab(tabName, btn) {
    document.querySelectorAll('.settings-tab-btn').forEach(b => b.classList.remove('active'));
    document.querySelectorAll('.settings-section').forEach(s => s.classList.remove('active'));
    btn.classList.add('active');
    const sec = document.getElementById('tab-' + tabName);
    if (sec) sec.classList.add('active');
    if (tabName === 'yaml') loadRawYaml();
  }

  function openModal(id) { document.getElementById(id).classList.add('active'); }
  function closeModal(id) { document.getElementById(id).classList.remove('active'); }

  // --- VIEW 1: Library ---
  async function downloadAllArtistsReleases() {
    if (!confirm('Check all monitored artists and queue all missing releases for download?')) {
      return;
    }
    const btn = document.getElementById('btn_download_all_artists');
    if (btn) {
      btn.disabled = true;
      btn.innerHTML = '<svg class="icon spin" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg> Checking & Queuing...';
    }
    showToast('Checking all artists for missing releases...', 'info');
    try {
      const res = await fetch('/api/artists/download-all', { method: 'POST' });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || data.error || 'Failed to queue downloads');
      showToast(data.message || 'Queued downloads for all artists', 'success');
      await loadLibraryArtists();
      if (typeof updateQueueCount === 'function') updateQueueCount();
    } catch (err) {
      showToast('Download All failed: ' + err.message, 'error');
    } finally {
      if (btn) {
        btn.disabled = false;
        btn.innerHTML = '<svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg> Download All Missing';
      }
    }
  }

  async function scanLibraryArtists() {
    const btn = document.getElementById('btn_scan_library');
    if (btn) {
      btn.disabled = true;
      btn.innerHTML = '<svg class="icon spin" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg> Scanning...';
    }
    showToast('Scanning existing music library for artists...', 'info');
    try {
      const res = await fetch('/api/library/scan', { method: 'POST' });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || data.error || 'Scan failed');
      showToast(data.message || 'Library scan completed', 'success');
      await loadLibraryArtists();
    } catch (err) {
      showToast('Library scan failed: ' + err.message, 'error');
    } finally {
      if (btn) {
        btn.disabled = false;
        btn.innerHTML = '<svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" y1="3" x2="12" y2="15"/></svg> Import From Library';
      }
    }
  }

  async function loadLibraryArtists() {
    try {
      const res = await fetch('/api/artists');
      allArtists = await res.json() || [];
      renderFilteredArtists();
    } catch (err) {
      showToast('Failed to load library: ' + err.message, 'error');
    }
  }

  function renderFilteredArtists() {
    const q = (document.getElementById('libraryFilter').value || '').toLowerCase().trim();
    const filtered = allArtists.filter(a => a.name.toLowerCase().includes(q));
    const grid = document.getElementById('libraryGrid');
    document.getElementById('artistCountSummary').innerText = filtered.length + ' artist' + (filtered.length === 1 ? '' : 's');

    if (!filtered.length) {
      grid.innerHTML = '<div style="color: var(--p-muted); padding: 60px 20px; text-align: center; grid-column: 1 / -1; background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">' +
        (allArtists.length === 0 ? 'No monitored artists yet. Use Search & Add to track musicians and download lossless audio.' : 'No artists match your filter.') +
        '</div>';
      return;
    }

    grid.innerHTML = filtered.map(a => {
      const img = a.image_url || 'data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" fill="%23171b28"><rect width="200" height="200"/><circle cx="100" cy="100" r="40" fill="%23252b3e"/><circle cx="100" cy="100" r="10" fill="%23090a0f"/></svg>';
      return '<div class="artist-card">' +
        '<div class="artist-cover-container" onclick="openArtistPage(\'' + a.id + '\')">' +
          '<img class="artist-cover" src="' + img + '" alt="' + a.name + '" loading="lazy">' +
          '<div class="artist-badge-top">' + (a.source || 'indexer') + '</div>' +
        '</div>' +
        '<div class="artist-content">' +
          '<h4 class="artist-name" title="' + a.name + '" onclick="openArtistPage(\'' + a.id + '\')" style="cursor: pointer;">' + a.name + '</h4>' +
          '<div class="artist-meta">' +
            '<span style="' + (a.monitored ? 'color: var(--p-success);' : 'color: var(--p-dim);') + '">' + (a.monitored ? 'Monitored' : 'Unmonitored') + '</span>' +
          '</div>' +
          '<div style="display: flex; gap: 8px; margin-top: auto;">' +
            '<button class="btn btn-secondary btn-sm" style="flex: 1;" onclick="openArtistPage(\'' + a.id + '\')">View Artist</button>' +
            '<button class="btn btn-sm btn-success" onclick="downloadArtistAll(\'' + a.id + '\', \'' + a.name.replace(/'/g, "\\'") + '\')" title="Download Complete Discography">' +
              '<svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 15V3m9 12v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/></svg>' +
              'Download All' +
            '</button>' +
            '<button class="btn btn-danger btn-sm" title="Remove artist" onclick="deleteArtist(\'' + a.id + '\', \'' + a.name.replace(/'/g, "\\'") + '\')">' +
              '<svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>' +
            '</button>' +
          '</div>' +
        '</div>' +
      '</div>';
    }).join('');
  }

  // --- VIEW: Dedicated Artist Page State & Controls ---
  let currentArtistData = null;
  let currentArtistAlbums = [];
  let currentArtistFilterCat = 'all';
  let currentArtistFilterText = '';
  let currentArtistViewMode = 'list';
  let isArtistPreviewMode = false;
  let currentPreviewPayload = null;
  let artistPreviousView = 'artists';
  let selectedAlbumIDs = new Set();

  async function openArtistPage(artistID, pushHash = true) {
    if (currentView !== 'artist') {
      artistPreviousView = currentView || 'artists';
    }
    isArtistPreviewMode = false;
    currentPreviewPayload = null;
    navigateTo('artist', false);

    if (pushHash) {
      window.location.hash = '#artist/' + encodeURIComponent(artistID);
    }

    document.getElementById('artistBackButtonLabel').innerText = 'Back to Library';
    document.getElementById('artistBreadcrumbRoot').innerText = 'Library';
    document.getElementById('artistBreadcrumbName').innerText = 'Loading...';
    document.getElementById('pageTitle').innerText = 'Library';
    document.getElementById('artistHeroName').innerText = 'Loading Artist...';
    document.getElementById('artistHeroOverview').style.display = 'none';
    document.getElementById('artistHeroPreviewBadge').style.display = 'none';
    document.getElementById('artistReleasesList').innerHTML =
      '<div style="text-align: center; padding: 70px 20px; color: var(--p-muted);">' +
        '<span class="pulse-dot" style="display: inline-block; vertical-align: middle; margin-right: 8px;"></span>' +
        'Loading artist discography...' +
      '</div>';

    try {
      const res = await fetch('/api/artists/' + artistID);
      if (!res.ok) throw new Error('Artist not found in library');
      const data = await res.json();
      currentArtistData = data.artist;
      currentArtistAlbums = data.albums || [];
      selectedAlbumIDs.clear();
      currentArtistFilterCat = 'all';
      currentArtistFilterText = '';
      const filterInput = document.getElementById('artistReleaseFilterInput');
      if (filterInput) filterInput.value = '';

      renderDedicatedArtistPage();
    } catch (err) {
      document.getElementById('artistReleasesList').innerHTML =
        '<div style="color: var(--p-danger); padding: 40px; text-align: center; background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">' +
          'Failed to load artist details: ' + err.message +
          '<div style="margin-top: 14px;"><button class="btn btn-secondary btn-sm" onclick="navigateBackFromArtist()">Back to Library</button></div>' +
        '</div>';
    }
  }

  async function openArtistPreviewPage(sourceID, jsonArtistEncoded) {
    artistPreviousView = 'search';
    isArtistPreviewMode = true;
    currentPreviewPayload = jsonArtistEncoded;
    const artist = JSON.parse(decodeURIComponent(jsonArtistEncoded));

    navigateTo('artist', false);
    window.location.hash = '#artist/preview-' + encodeURIComponent(sourceID);

    document.getElementById('artistBackButtonLabel').innerText = 'Back to Search';
    document.getElementById('artistBreadcrumbRoot').innerText = 'Search';
    document.getElementById('artistBreadcrumbName').innerText = 'Preview: ' + artist.name;
    document.getElementById('pageTitle').innerText = 'Preview: ' + artist.name;
    document.getElementById('artistHeroName').innerText = artist.name;
    document.getElementById('artistHeroOverview').style.display = 'none';
    document.getElementById('artistHeroPreviewBadge').style.display = 'inline-block';
    document.getElementById('artistReleasesList').innerHTML =
      '<div style="text-align: center; padding: 70px 20px; color: var(--p-muted);">' +
        '<span class="pulse-dot" style="display: inline-block; vertical-align: middle; margin-right: 8px;"></span>' +
        'Fetching catalog preview from indexer...' +
      '</div>';

    try {
      const res = await fetch('/api/search/preview?artist_id=' + sourceID);
      const albums = await res.json() || [];
      currentArtistData = {
        id: sourceID,
        source_id: sourceID,
        source: artist.source || 'indexer',
        name: artist.name,
        image_url: artist.image_url,
        monitored: false,
        isPreview: true
      };
      currentArtistAlbums = albums.map((alb, idx) => ({
        ...alb,
        id: alb.id || ('preview-' + idx),
        status: 'missing',
        monitored: false
      }));
      selectedAlbumIDs.clear();
      currentArtistFilterCat = 'all';
      currentArtistFilterText = '';
      const filterInput = document.getElementById('artistReleaseFilterInput');
      if (filterInput) filterInput.value = '';

      renderDedicatedArtistPage();
    } catch (err) {
      document.getElementById('artistReleasesList').innerHTML =
        '<div style="color: var(--p-danger); padding: 40px; text-align: center; background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">' +
          'Failed to load catalog preview: ' + err.message +
          '<div style="margin-top: 14px;"><button class="btn btn-secondary btn-sm" onclick="navigateTo(\'search\')">Back to Search</button></div>' +
        '</div>';
    }
  }

  function navigateBackFromArtist() {
    if (isArtistPreviewMode || artistPreviousView === 'search') {
      navigateTo('search');
    } else {
      navigateTo('artists');
    }
  }

  function setArtistViewMode(mode) {
    currentArtistViewMode = mode;
    document.getElementById('btnToggleList').classList.toggle('active', mode === 'list');
    document.getElementById('btnToggleGrid').classList.toggle('active', mode === 'grid');
    renderArtistReleases();
  }

  function setArtistFilterCat(cat) {
    currentArtistFilterCat = cat;
    renderDedicatedArtistPage();
  }

  function onArtistReleaseFilter(val) {
    currentArtistFilterText = val;
    renderArtistReleases();
  }

  function renderDedicatedArtistPage() {
    if (!currentArtistData) return;

    // Breadcrumbs & Titles
    if (isArtistPreviewMode) {
      document.getElementById('artistBackButtonLabel').innerText = 'Back to Search';
      document.getElementById('artistBreadcrumbRoot').innerText = 'Search';
      document.getElementById('artistBreadcrumbName').innerText = 'Preview: ' + currentArtistData.name;
      document.getElementById('pageTitle').innerText = 'Preview: ' + currentArtistData.name;
    } else {
      document.getElementById('artistBackButtonLabel').innerText = 'Back to Library';
      document.getElementById('artistBreadcrumbRoot').innerText = 'Library';
      document.getElementById('artistBreadcrumbName').innerText = currentArtistData.name;
      document.getElementById('pageTitle').innerText = currentArtistData.name;
    }

    // Hero Media & Info
    const img = currentArtistData.image_url || 'data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" fill="%23171b28"><rect width="200" height="200"/><circle cx="100" cy="100" r="40" fill="%23252b3e"/><circle cx="100" cy="100" r="10" fill="%23090a0f"/></svg>';
    document.getElementById('artistHeroBackdrop').style.backgroundImage = 'url("' + img + '")';
    document.getElementById('artistHeroImg').src = img;
    document.getElementById('artistHeroImg').alt = currentArtistData.name;
    document.getElementById('artistHeroName').innerText = currentArtistData.name;
    document.getElementById('artistHeroSourceTag').innerText = (currentArtistData.source || 'indexer').toUpperCase();

    const monPill = document.getElementById('artistHeroMonitoredPill');
    const prevBadge = document.getElementById('artistHeroPreviewBadge');
    if (isArtistPreviewMode) {
      monPill.style.display = 'none';
      prevBadge.style.display = 'inline-block';
    } else {
      prevBadge.style.display = 'none';
      monPill.style.display = 'inline-block';
      if (currentArtistData.monitored) {
        monPill.className = 'status-pill status-completed';
        monPill.innerText = 'Monitored';
      } else {
        monPill.className = 'status-pill status-missing';
        monPill.innerText = 'Unmonitored';
      }
    }

    const bioEl = document.getElementById('artistHeroOverview');
    if (currentArtistData.overview) {
      bioEl.style.display = 'block';
      bioEl.innerText = currentArtistData.overview;
    } else {
      bioEl.style.display = 'none';
    }

    // Stats Ribbon
    const totalCount = currentArtistAlbums.length;
    const downloadedCount = currentArtistAlbums.filter(a => a.status === 'completed').length;
    const missingCount = currentArtistAlbums.filter(a => a.status === 'missing' || a.status === 'pending-download' || !a.status).length;
    const monitoredCount = currentArtistAlbums.filter(a => a.monitored).length;

    document.getElementById('artistStatTotal').innerText = totalCount;
    document.getElementById('artistStatDownloaded').innerText = downloadedCount;
    document.getElementById('artistStatMissing').innerText = missingCount;
    document.getElementById('artistStatMonitored').innerText = monitoredCount;

    // Action Buttons
    const actionsContainer = document.getElementById('artistHeroActions');
    if (isArtistPreviewMode) {
      actionsContainer.innerHTML =
        '<button class="btn btn-success" onclick="addArtistFromSearch(\'' + currentPreviewPayload + '\', true)">' +
          '<svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 15V3m9 12v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/></svg>' +
          'Track & Download Everything' +
        '</button>' +
        '<button class="btn btn-secondary" onclick="addArtistFromSearch(\'' + currentPreviewPayload + '\', false)">' +
          'Add & Monitor' +
        '</button>';
    } else {
      actionsContainer.innerHTML =
        '<button class="btn btn-success" onclick="downloadCurrentArtistAll()">' +
          '<svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 15V3m9 12v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/></svg>' +
          'Download Discography' +
        '</button>' +
        '<button class="btn btn-secondary" id="btnArtistToggleMonitored" onclick="toggleCurrentArtistMonitored()">' +
          (currentArtistData.monitored ? 'Unmonitor Artist' : 'Monitor Artist') +
        '</button>' +
        '<button class="btn btn-secondary" onclick="reloadCurrentArtistPage()" title="Refresh discography from database">' +
          '<svg class="icon" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>' +
          'Refresh' +
        '</button>' +
        '<button class="btn btn-danger btn-sm" onclick="deleteCurrentArtist()" title="Remove artist and releases from library">' +
          '<svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2"><path d="M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>' +
          'Delete Artist' +
        '</button>';
    }

    // Category Tabs
    const albumList = currentArtistAlbums.filter(a => (a.release_type || 'album').toLowerCase() === 'album');
    const epList = currentArtistAlbums.filter(a => (a.release_type || '').toLowerCase() === 'ep');
    const singleList = currentArtistAlbums.filter(a => (a.release_type || '').toLowerCase() === 'single');
    const otherList = currentArtistAlbums.filter(a => {
      const t = (a.release_type || '').toLowerCase();
      return t !== 'album' && t !== 'ep' && t !== 'single';
    });

    let tabsHtml = '';
    tabsHtml += '<button class="discog-tab-btn ' + (currentArtistFilterCat === 'all' ? 'active' : '') + '" onclick="setArtistFilterCat(\'all\')">All Releases <span class="discog-counter">' + totalCount + '</span></button>';
    tabsHtml += '<button class="discog-tab-btn ' + (currentArtistFilterCat === 'album' ? 'active' : '') + '" onclick="setArtistFilterCat(\'album\')">Albums <span class="discog-counter">' + albumList.length + '</span></button>';
    tabsHtml += '<button class="discog-tab-btn ' + (currentArtistFilterCat === 'ep' ? 'active' : '') + '" onclick="setArtistFilterCat(\'ep\')">EPs <span class="discog-counter">' + epList.length + '</span></button>';
    tabsHtml += '<button class="discog-tab-btn ' + (currentArtistFilterCat === 'single' ? 'active' : '') + '" onclick="setArtistFilterCat(\'single\')">Singles <span class="discog-counter">' + singleList.length + '</span></button>';
    if (otherList.length > 0) {
      tabsHtml += '<button class="discog-tab-btn ' + (currentArtistFilterCat === 'other' ? 'active' : '') + '" onclick="setArtistFilterCat(\'other\')">Other <span class="discog-counter">' + otherList.length + '</span></button>';
    }
    document.getElementById('artistCategoryTabs').innerHTML = tabsHtml;

    // Render Releases
    renderArtistReleases();
  }

  function getFilteredArtistAlbums() {
    let list = currentArtistAlbums;
    if (currentArtistFilterCat === 'album') list = list.filter(a => (a.release_type || 'album').toLowerCase() === 'album');
    else if (currentArtistFilterCat === 'ep') list = list.filter(a => (a.release_type || '').toLowerCase() === 'ep');
    else if (currentArtistFilterCat === 'single') list = list.filter(a => (a.release_type || '').toLowerCase() === 'single');
    else if (currentArtistFilterCat === 'other') list = list.filter(a => {
      const t = (a.release_type || '').toLowerCase();
      return t !== 'album' && t !== 'ep' && t !== 'single';
    });

    if (currentArtistFilterText) {
      const q = currentArtistFilterText.toLowerCase().trim();
      list = list.filter(a => (a.title || '').toLowerCase().includes(q) || (a.year ? ('' + a.year).includes(q) : false));
    }
    return list;
  }

  function renderArtistBatchToolbar(displayed) {
    const toolbar = document.getElementById('artistBatchToolbar');
    if (!toolbar) return;

    if (isArtistPreviewMode) {
      toolbar.style.display = 'none';
      return;
    }
    toolbar.style.display = 'flex';

    const allSelectedInView = displayed.length > 0 && displayed.every(a => selectedAlbumIDs.has(a.id));
    const selCount = selectedAlbumIDs.size;

    toolbar.innerHTML =
      '<div style="display: flex; align-items: center; gap: 8px;">' +
        '<input type="checkbox" id="selectAllPageCb" ' + (allSelectedInView ? 'checked' : '') + ' onchange="toggleSelectAllInPage(this.checked)">' +
        '<label for="selectAllPageCb" style="cursor: pointer; font-weight: 500;">Select All in View</label>' +
        (selCount > 0 ? ('<span style="font-family: var(--font-mono); font-size: 12px; color: var(--p-accent); margin-left: 6px;">(' + selCount + ' selected)</span>') : '') +
      '</div>' +
      '<div style="display: flex; gap: 8px; align-items: center;">' +
        '<button class="btn btn-sm btn-success" ' + (selCount === 0 ? 'disabled style="opacity: 0.5;"' : '') + ' onclick="executeBatchAlbumActionFromPage(\'download\')">' +
          '<svg class="icon" viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M12 15V3m9 12v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/></svg>' +
          'Download Selected (' + selCount + ')' +
        '</button>' +
        '<button class="btn btn-secondary btn-sm" ' + (selCount === 0 ? 'disabled style="opacity: 0.5;"' : '') + ' onclick="executeBatchAlbumActionFromPage(\'monitor\')">Monitor</button>' +
        '<button class="btn btn-secondary btn-sm" ' + (selCount === 0 ? 'disabled style="opacity: 0.5;"' : '') + ' onclick="executeBatchAlbumActionFromPage(\'unmonitor\')">Unmonitor</button>' +
      '</div>';
  }

  function renderArtistReleases() {
    const displayed = getFilteredArtistAlbums();
    renderArtistBatchToolbar(displayed);

    const container = document.getElementById('artistReleasesList');
    if (!displayed.length) {
      container.innerHTML =
        '<div style="text-align: center; padding: 60px 20px; color: var(--p-muted); background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">' +
          (currentArtistAlbums.length === 0 ? 'No releases indexed for this artist.' : 'No releases match your filter.') +
        '</div>';
      return;
    }

    if (currentArtistViewMode === 'grid') {
      container.innerHTML = '<div class="discog-grid">' + displayed.map(alb => {
        const cover = alb.cover_url || 'data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" fill="%23171b28"><rect width="200" height="200"/><circle cx="100" cy="100" r="40" fill="%23252b3e"/></svg>';
        const isSelected = selectedAlbumIDs.has(alb.id);
        const statusClass = 'status-' + (alb.status || 'missing');
        const trackCount = (alb.tracks && alb.tracks.length) ? alb.tracks.length : (alb.total_tracks || '-');

        return '<div class="album-card-grid" id="grid-card-' + alb.id + '">' +
          '<div class="album-grid-cover-wrap">' +
            '<img class="album-grid-cover" src="' + cover + '" alt="' + alb.title + '" loading="lazy">' +
            '<span class="status-pill ' + statusClass + ' album-grid-badge">' + (alb.status || 'missing') + '</span>' +
          '</div>' +
          '<div class="album-grid-info">' +
            '<div style="display: flex; align-items: center; justify-content: space-between; gap: 6px; margin-bottom: 4px;">' +
              '<span class="code-tag" style="text-transform: uppercase; font-size: 9.5px;">' + (alb.release_type || 'album') + '</span>' +
              '<span class="lossless-pill">FLAC</span>' +
            '</div>' +
            '<h4 class="album-grid-title" title="' + alb.title + '">' + alb.title + '</h4>' +
            '<div class="album-grid-meta">' + (alb.year || 'N/A') + ' • ' + trackCount + ' tracks' + '</div>' +
            (!isArtistPreviewMode ? (
              '<div class="album-grid-actions">' +
                '<button class="btn btn-secondary btn-sm" style="flex: 1;" onclick="downloadSingleAlbumFromPage(\'' + alb.id + '\', \'' + alb.title.replace(/'/g, "\\'") + '\')">' +
                  (alb.status === 'completed' ? 'Re-Download' : 'Download') +
                '</button>' +
                '<button class="btn btn-secondary btn-sm" onclick="toggleAlbumTracksFromPage(\'' + alb.id + '\')" title="View Tracks">' +
                  'Tracks' +
                '</button>' +
              '</div>'
            ) : '') +
          '</div>' +
          '<div class="release-tracklist" id="page-tracklist-' + alb.id + '"></div>' +
        '</div>';
      }).join('') + '</div>';
    } else {
      container.innerHTML = displayed.map(alb => {
        const cover = alb.cover_url || 'data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" fill="%23171b28"><rect width="200" height="200"/><circle cx="100" cy="100" r="40" fill="%23252b3e"/></svg>';
        const isSelected = selectedAlbumIDs.has(alb.id);
        const statusClass = 'status-' + (alb.status || 'missing');
        const trackCount = (alb.tracks && alb.tracks.length) ? alb.tracks.length : (alb.total_tracks || '-');

        return '<div class="release-card" id="page-card-' + alb.id + '">' +
          '<div class="release-header" onclick="toggleAlbumTracksFromPage(\'' + alb.id + '\')">' +
            (!isArtistPreviewMode ? (
              '<input type="checkbox" ' + (isSelected ? 'checked' : '') + ' onclick="event.stopPropagation()" onchange="toggleSelectAlbumInPage(\'' + alb.id + '\', this.checked)">'
            ) : '') +
            '<img src="' + cover + '" style="width: 48px; height: 48px; border-radius: 6px; object-fit: cover; background: var(--p-surface-0); border: 1px solid var(--p-border); flex-shrink: 0;">' +
            '<div style="flex: 1; min-width: 0;">' +
              '<div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap;">' +
                '<span style="font-weight: 600; font-size: 15px; color: var(--p-ink);">' + alb.title + '</span>' +
                '<span class="code-tag" style="text-transform: uppercase; font-size: 10px;">' + (alb.release_type || 'album') + '</span>' +
                '<span class="lossless-pill">FLAC LOSSLESS</span>' +
              '</div>' +
              '<div style="font-size: 12.5px; color: var(--p-muted); font-family: var(--font-mono); margin-top: 3px;">' +
                (alb.year || 'N/A') + ' • ' + trackCount + ' tracks' + (alb.release_date ? ' • ' + alb.release_date : '') +
              '</div>' +
            '</div>' +
            '<div style="display: flex; align-items: center; gap: 10px;" onclick="event.stopPropagation()">' +
              (!isArtistPreviewMode ? (
                '<label class="switch-label" title="Toggle Monitoring" style="font-size: 12px;">' +
                  '<input type="checkbox" ' + (alb.monitored ? 'checked' : '') + ' onchange="toggleAlbumMonitoredFromPage(\'' + alb.id + '\', this.checked)">' +
                  '<span style="color: var(--p-muted); font-size: 12px;">Monitor</span>' +
                '</label>' +
                '<span class="status-pill ' + statusClass + '">' + (alb.status || 'missing') + '</span>' +
                '<button class="btn btn-secondary btn-sm" onclick="downloadSingleAlbumFromPage(\'' + alb.id + '\', \'' + alb.title.replace(/'/g, "\\'") + '\')">' +
                  (alb.status === 'completed' ? 'Re-Download' : 'Download') +
                '</button>'
              ) : '<span class="status-pill status-missing">Unmonitored</span>') +
              '<button class="btn btn-secondary btn-sm" onclick="toggleAlbumTracksFromPage(\'' + alb.id + '\')" title="View Tracks">' +
                '<span id="page-chevron-' + alb.id + '">▼</span> Tracks' +
              '</button>' +
            '</div>' +
          '</div>' +
          '<div class="release-tracklist" id="page-tracklist-' + alb.id + '"></div>' +
        '</div>';
      }).join('');
    }
  }

  function toggleSelectAllInPage(checked) {
    const displayed = getFilteredArtistAlbums();
    displayed.forEach(a => {
      if (checked) selectedAlbumIDs.add(a.id);
      else selectedAlbumIDs.delete(a.id);
    });
    renderArtistReleases();
  }

  function toggleSelectAlbumInPage(id, checked) {
    if (checked) selectedAlbumIDs.add(id);
    else selectedAlbumIDs.delete(id);
    renderArtistBatchToolbar(getFilteredArtistAlbums());
  }

  async function executeBatchAlbumActionFromPage(action) {
    if (selectedAlbumIDs.size === 0) {
      showToast('No releases selected. Check the boxes next to releases.', 'info');
      return;
    }
    const ids = Array.from(selectedAlbumIDs);
    showToast('Applying ' + action + ' to ' + ids.length + ' releases...', 'info');
    try {
      const res = await fetch('/api/albums/batch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: action, album_ids: ids })
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || 'Batch action failed');
      showToast(data.message || 'Batch action completed', 'success');

      if (action === 'monitor' || action === 'unmonitor') {
        const monitored = action === 'monitor';
        currentArtistAlbums.forEach(a => {
          if (selectedAlbumIDs.has(a.id)) a.monitored = monitored;
        });
        renderDedicatedArtistPage();
      } else if (action === 'download') {
        currentArtistAlbums.forEach(a => {
          if (selectedAlbumIDs.has(a.id)) a.status = 'pending';
        });
        renderDedicatedArtistPage();
        loadLiveQueue();
      }
    } catch (err) {
      showToast(err.message, 'error');
    }
  }

  async function downloadSingleAlbumFromPage(albumID, albumTitle) {
    showToast('Queueing download for ' + albumTitle + '...', 'info');
    try {
      await fetch('/api/albums/' + albumID + '/download', { method: 'POST' });
      showToast('Download pipeline queued for ' + albumTitle, 'success');
      const alb = currentArtistAlbums.find(a => a.id === albumID);
      if (alb) {
        alb.status = 'pending';
        renderDedicatedArtistPage();
      }
      loadLiveQueue();
    } catch (err) {
      showToast('Album download error: ' + err.message, 'error');
    }
  }

  async function toggleAlbumMonitoredFromPage(albumID, monitored) {
    try {
      const res = await fetch('/api/albums/' + albumID, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ monitored: monitored })
      });
      if (!res.ok) throw new Error('Failed to update album monitoring');
      showToast('Album monitoring ' + (monitored ? 'enabled' : 'disabled'), 'success');
      const alb = currentArtistAlbums.find(a => a.id === albumID);
      if (alb) alb.monitored = monitored;
      renderDedicatedArtistPage();
    } catch (err) {
      showToast(err.message, 'error');
    }
  }

  async function toggleCurrentArtistMonitored() {
    if (!currentArtistData || !currentArtistData.id) return;
    const newMonitored = !currentArtistData.monitored;
    try {
      const res = await fetch('/api/artists/' + currentArtistData.id, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ monitored: newMonitored })
      });
      if (!res.ok) throw new Error('Failed to update artist monitoring');
      currentArtistData.monitored = newMonitored;
      showToast('Artist ' + (newMonitored ? 'monitored' : 'unmonitored'), 'success');
      renderDedicatedArtistPage();
      loadLibraryArtists();
    } catch (err) {
      showToast(err.message, 'error');
    }
  }

  async function downloadCurrentArtistAll() {
    if (!currentArtistData || !currentArtistData.id) return;
    showToast('Initiating discography download for ' + currentArtistData.name + '...', 'info');
    try {
      await fetch('/api/artists/' + currentArtistData.id + '/download-all', { method: 'POST' });
      showToast('Started background download for ' + currentArtistData.name, 'success');
      currentArtistAlbums.forEach(a => {
        if (a.status !== 'completed') a.status = 'pending';
      });
      renderDedicatedArtistPage();
      loadLiveQueue();
    } catch (err) {
      showToast('Download error: ' + err.message, 'error');
    }
  }

  async function deleteCurrentArtist() {
    if (!currentArtistData || !currentArtistData.id) return;
    if (!confirm('Are you sure you want to delete ' + currentArtistData.name + ' and all indexed releases?')) return;
    try {
      await fetch('/api/artists/' + currentArtistData.id, { method: 'DELETE' });
      showToast('Removed ' + currentArtistData.name, 'success');
      loadLibraryArtists();
      navigateBackFromArtist();
    } catch (err) {
      showToast('Delete error: ' + err.message, 'error');
    }
  }

  async function reloadCurrentArtistPage() {
    if (currentArtistData && currentArtistData.id && !isArtistPreviewMode) {
      await openArtistPage(currentArtistData.id, false);
      showToast('Refreshed ' + currentArtistData.name, 'info');
    }
  }

  async function toggleAlbumTracksFromPage(albumID) {
    const el = document.getElementById('page-tracklist-' + albumID);
    const chev = document.getElementById('page-chevron-' + albumID);
    if (!el) return;

    if (el.classList.contains('open')) {
      el.classList.remove('open');
      if (chev) chev.innerText = '▼';
      return;
    }

    el.classList.add('open');
    if (chev) chev.innerText = '▲';

    const alb = currentArtistAlbums.find(a => a.id === albumID);
    if (!alb) return;

    if (!alb.tracks || alb.tracks.length === 0) {
      el.innerHTML = '<div style="padding: 16px; text-align: center; color: var(--p-muted); font-size: 13px;">' +
        '<span class="pulse-dot" style="display: inline-block; vertical-align: middle; margin-right: 6px;"></span> Fetching tracks from catalog...</div>';
      try {
        const res = await fetch('/api/albums/' + albumID);
        const detailed = await res.json();
        alb.tracks = detailed.tracks || [];
        alb.total_tracks = detailed.total_tracks || alb.tracks.length;
      } catch (err) {
        el.innerHTML = '<div style="color: var(--p-danger); padding: 12px;">Failed to load tracks: ' + err.message + '</div>';
        return;
      }
    }

    renderAlbumTracklist(albumID, alb.tracks, el);
  }

  // Backwards compatibility wrappers
  async function viewArtistDetail(artistID) {
    openArtistPage(artistID);
  }

  async function previewArtistDiscography(sourceID, jsonArtistEncoded) {
    openArtistPreviewPage(sourceID, jsonArtistEncoded);
  }

  function renderAlbumTracklist(albumID, tracks, targetEl) {
    const el = targetEl || document.getElementById('tracklist-' + albumID) || document.getElementById('page-tracklist-' + albumID);
    if (!el) return;

    if (!tracks || tracks.length === 0) {
      el.innerHTML = '<div style="padding: 14px; text-align: center; color: var(--p-muted); font-size: 13px;">No track information cataloged.</div>';
      return;
    }

    let html = '<table style="margin: 4px 0; width: 100%;">' +
      '<thead><tr>' +
        '<th style="width: 50px;">#</th>' +
        '<th>Title</th>' +
        '<th>Duration</th>' +
        '<th>Audio Quality</th>' +
        '<th>Explicit</th>' +
        '<th>ISRC</th>' +
      '</tr></thead><tbody>';

    html += tracks.map((t, idx) => {
      const trackNum = t.track_number || (idx + 1);
      const discPrefix = (t.disc_number && t.disc_number > 1) ? (t.disc_number + '-') : '';
      const displayNum = discPrefix + (trackNum < 10 ? '0' + trackNum : trackNum);
      const durFormatted = formatDuration(t.duration_sec || t.duration);
      const explicitBadge = t.explicit ? '<span style="background: rgba(239, 68, 68, 0.15); color: #fca5a5; font-size: 10px; font-family: var(--font-mono); padding: 2px 6px; border-radius: 4px; font-weight: 600;">EXPLICIT</span>' : '-';
      const isrc = t.isrc ? '<span style="font-family: var(--font-mono); font-size: 11px; color: var(--p-muted);">' + t.isrc + '</span>' : '-';

      return '<tr>' +
        '<td style="font-family: var(--font-mono); color: var(--p-muted);">' + displayNum + '</td>' +
        '<td style="font-weight: 500;">' + t.title + '</td>' +
        '<td style="font-family: var(--font-mono); font-size: 12px; color: var(--p-muted);">' + durFormatted + '</td>' +
        '<td><span class="lossless-pill">FLAC 24-BIT</span></td>' +
        '<td>' + explicitBadge + '</td>' +
        '<td>' + isrc + '</td>' +
      '</tr>';
    }).join('');

    html += '</tbody></table>';
    el.innerHTML = html;
  }

  function formatDuration(sec) {
    if (!sec || sec <= 0) return '--:--';
    const m = Math.floor(sec / 60);
    const s = Math.floor(sec % 60);
    return m + ':' + (s < 10 ? '0' + s : s);
  }

  async function downloadArtistAll(artistID, artistName) {
    showToast('Initiating discography download for ' + artistName + '...', 'info');
    try {
      await fetch('/api/artists/' + artistID + '/download-all', { method: 'POST' });
      showToast('Started background download for ' + artistName, 'success');
    } catch (err) {
      showToast('Download error: ' + err.message, 'error');
    }
  }

  async function downloadSingleAlbum(albumID, albumTitle) {
    downloadSingleAlbumFromPage(albumID, albumTitle);
  }

  async function deleteArtist(artistID, artistName) {
    if (!confirm('Are you sure you want to delete ' + artistName + ' and all indexed releases?')) return;
    try {
      await fetch('/api/artists/' + artistID, { method: 'DELETE' });
      showToast('Removed ' + artistName, 'success');
      closeModal('artistDetailModal');
      loadLibraryArtists();
      if (currentView === 'artist') {
        navigateBackFromArtist();
      }
    } catch (err) {
      showToast('Delete error: ' + err.message, 'error');
    }
  }

  // --- VIEW 2: Search & Add Artists ---
  function quickSearch(name) {
    document.getElementById('searchQueryInput').value = name;
    executeArtistSearch();
  }

  async function executeArtistSearch() {
    const q = document.getElementById('searchQueryInput').value.trim();
    if (!q) return;

    const provider = document.getElementById('searchProviderSelect').value;
    const grid = document.getElementById('searchResultsGrid');
    const countEl = document.getElementById('searchResultsCount');
    grid.innerHTML = '<div style="grid-column: 1 / -1; padding: 60px 20px; text-align: center; color: var(--p-muted); background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">' +
      'Searching ' + provider.toUpperCase() + ' catalog for "<strong>' + q + '</strong>"...</div>';

    try {
      const res = await fetch('/api/search?q=' + encodeURIComponent(q) + '&provider=' + provider);
      const artists = await res.json() || [];
      countEl.innerText = artists.length + ' match' + (artists.length === 1 ? '' : 'es') + ' found';

      if (!artists.length) {
        grid.innerHTML = '<div style="grid-column: 1 / -1; padding: 60px 20px; text-align: center; color: var(--p-muted); background: var(--p-surface-1); border: 1px dashed var(--p-border); border-radius: var(--r-md);">' +
          'No artists found for "' + q + '". Try another spelling or switch catalog provider.' +
          '</div>';
        return;
      }

      grid.innerHTML = artists.map(a => {
        const img = a.image_url || 'data:image/svg+xml;utf8,<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200" fill="%23171b28"><rect width="200" height="200"/><circle cx="100" cy="100" r="40" fill="%23252b3e"/><circle cx="100" cy="100" r="10" fill="%23090a0f"/></svg>';
        const inLib = allArtists.find(x => (x.source_id && x.source_id === a.source_id) || (x.name && x.name.toLowerCase() === a.name.toLowerCase()));
        const inLibraryBadge = inLib ?
          '<span style="background: var(--p-success-subtle); color: var(--p-success); font-family: var(--font-mono); font-size: 10.5px; padding: 2px 6px; border-radius: 4px; font-weight: 500;">✓ In Library</span>' : '';

        const jsonPayload = encodeURIComponent(JSON.stringify(a));
        const clickAction = inLib ? ("openArtistPage('" + inLib.id + "')") : ("openArtistPreviewPage('" + a.source_id + "', '" + jsonPayload + "')");

        return '<div class="artist-card">' +
          '<div class="artist-cover-container" onclick="' + clickAction + '">' +
            '<img class="artist-cover" src="' + img + '" alt="' + a.name + '" loading="lazy">' +
            '<div class="artist-badge-top">' + (a.source || provider) + '</div>' +
          '</div>' +
          '<div class="artist-content">' +
            '<h4 class="artist-name" title="' + a.name + '" onclick="' + clickAction + '" style="cursor: pointer;">' + a.name + '</h4>' +
            '<div class="artist-meta">' +
              '<span>ID: ' + a.source_id + '</span>' +
              inLibraryBadge +
            '</div>' +
            '<div style="display: flex; flex-direction: column; gap: 8px; margin-top: auto;">' +
              (inLib ?
                '<button class="btn btn-secondary btn-sm" onclick="openArtistPage(\'' + inLib.id + '\')">View in Library</button>' :
                '<button class="btn btn-secondary btn-sm" onclick="openArtistPreviewPage(\'' + a.source_id + '\', \'' + jsonPayload + '\')">Preview Releases</button>'
              ) +
              '<div style="display: flex; gap: 6px;">' +
                '<button class="btn btn-sm" style="flex: 1;" onclick="addArtistFromSearch(\'' + jsonPayload + '\', false)">Add & Monitor</button>' +
                '<button class="btn btn-sm btn-success" style="flex: 1;" onclick="addArtistFromSearch(\'' + jsonPayload + '\', true)">Track & Get All</button>' +
              '</div>' +
            '</div>' +
          '</div>' +
        '</div>';
      }).join('');
    } catch (err) {
      grid.innerHTML = '<div style="color: var(--p-danger); padding: 30px; text-align: center;">Search failed: ' + err.message + '</div>';
    }
  }

  async function previewArtistDiscography(sourceID, jsonArtistEncoded) {
    const artist = JSON.parse(decodeURIComponent(jsonArtistEncoded));
    openModal('artistDetailModal');
    const body = document.getElementById('modalArtistBody');
    const title = document.getElementById('modalArtistTitle');
    title.innerText = 'Preview: ' + artist.name;
    body.innerHTML = '<div style="text-align: center; padding: 40px; color: var(--p-muted);">Fetching discography preview from indexer...</div>';

    try {
      const res = await fetch('/api/search/preview?artist_id=' + sourceID);
      const albums = await res.json() || [];

      let html = '<div style="display: flex; gap: 20px; align-items: center; margin-bottom: 24px;">' +
        '<img src="' + (artist.image_url || '') + '" style="width: 72px; height: 72px; border-radius: var(--r-sm); object-fit: cover; background: var(--p-surface-0); border: 1px solid var(--p-border);">' +
        '<div>' +
          '<h2 style="margin: 0 0 4px; font-size: 19px; font-weight: 700;">' + artist.name + '</h2>' +
          '<div style="color: var(--p-muted); font-size: 13px; font-family: var(--font-mono);">Indexer Preview • ' + albums.length + ' Releases found</div>' +
        '</div>' +
      '</div>' +
      '<div style="overflow-x: auto;">' +
        '<table>' +
          '<thead>' +
            '<tr>' +
              '<th style="width: 44px;"></th>' +
              '<th>Title</th>' +
              '<th>Type</th>' +
              '<th>Year</th>' +
            '</tr>' +
          '</thead>' +
          '<tbody>';

      albums.forEach(alb => {
        html += '<tr>' +
          '<td><img src="' + (alb.cover_url || '') + '" style="width: 36px; height: 36px; border-radius: 4px; object-fit: cover; background: var(--p-surface-0);"></td>' +
          '<td style="font-weight: 600;">' + alb.title + '</td>' +
          '<td><span style="font-family: var(--font-mono); font-size: 10.5px; background: var(--p-surface-0); border: 1px solid var(--p-border); padding: 2px 6px; border-radius: 4px; text-transform: uppercase;">' + (alb.release_type || 'album') + '</span></td>' +
          '<td style="font-family: var(--font-mono);">' + (alb.year || '-') + '</td>' +
        '</tr>';
      });

      html += '</tbody></table></div>';
      body.innerHTML = html;

      document.getElementById('modalArtistFooter').innerHTML =
        '<button class="btn btn-secondary btn-sm" onclick="closeModal(\'artistDetailModal\')">Close</button>' +
        '<button class="btn btn-sm" onclick="addArtistFromSearch(\'' + jsonArtistEncoded + '\', false); closeModal(\'artistDetailModal\');">Add & Monitor</button>' +
        '<button class="btn btn-success btn-sm" onclick="addArtistFromSearch(\'' + jsonArtistEncoded + '\', true); closeModal(\'artistDetailModal\');">Track & Download Everything</button>';
    } catch (err) {
      body.innerHTML = '<div style="color: var(--p-danger); padding: 20px;">Preview error: ' + err.message + '</div>';
    }
  }

  async function addArtistFromSearch(jsonArtistEncoded, downloadAll) {
    const artist = JSON.parse(decodeURIComponent(jsonArtistEncoded));
    showToast('Adding ' + artist.name + ' to library...', 'info');

    try {
      const res = await fetch('/api/artists', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          id: artist.id,
          source_id: artist.source_id,
          source: artist.source,
          name: artist.name,
          image_url: artist.image_url,
          monitored: true,
          download_all: downloadAll
        })
      });

      const data = await res.json();
      if (!res.ok) throw new Error(data.message || 'Error adding artist');

      showToast(downloadAll ? ('Added ' + artist.name + ' and started downloading discography') : ('Added ' + artist.name), 'success');
      executeArtistSearch();
    } catch (err) {
      showToast('Error: ' + err.message, 'error');
    }
  }

  // --- VIEW 3: Queue & Download History ---
  let currentHistoryFilter = 'all';
  let currentActivityStatus = 'all';

  async function loadLiveQueue() {
    const container = document.getElementById('liveQueueContainer');
    const badge = document.getElementById('queueBadge');
    const sideBadge = document.getElementById('sidebarQueueCount');
    const concurrencyDisplay = document.getElementById('queueConcurrencyDisplay');
    if (!container) return;

    try {
      const [resJobs, resStatus] = await Promise.all([
        fetch('/api/queue'),
        fetch('/api/queue/status').catch(() => null)
      ]);

      const jobs = await resJobs.json() || [];
      let qStatus = { max_concurrent: 2, active_downloads: 0, pending_downloads: 0 };
      if (resStatus && resStatus.ok) {
        try { qStatus = await resStatus.json(); } catch (e) {}
      }

      if (concurrencyDisplay) {
        concurrencyDisplay.innerText = 'Max Concurrent: ' + (qStatus.max_concurrent || 2) +
          ' (' + (qStatus.active_downloads || 0) + ' active, ' + (qStatus.pending_downloads || 0) + ' queued)';
      }

      if (badge) {
        badge.innerText = jobs.length + ' Total (' + (qStatus.active_downloads || 0) + ' Active)';
        badge.style.background = jobs.length > 0 ? 'var(--p-accent-subtle)' : 'var(--p-surface-0)';
        badge.style.borderColor = jobs.length > 0 ? 'var(--p-accent)' : 'var(--p-border)';
        badge.style.color = jobs.length > 0 ? 'var(--p-accent)' : 'var(--p-muted)';
      }
      if (sideBadge) {
        sideBadge.innerText = jobs.length;
        if (jobs.length > 0) sideBadge.classList.add('active-pulse');
        else sideBadge.classList.remove('active-pulse');
      }

      if (!jobs.length) {
        container.innerHTML = '<div style="background: var(--p-surface-0); border: 1px dashed var(--p-border); border-radius: var(--r-sm); padding: 24px; text-align: center; color: var(--p-muted); font-size: 13px;">' +
          'Download pipeline is idle. No transfers in progress.</div>';
        return;
      }

      const activeJobs = jobs.filter(j => j.status !== 'pending');
      const pendingJobs = jobs.filter(j => j.status === 'pending');

      let html = '';

      if (activeJobs.length > 0) {
        html += '<div style="font-size: 11px; font-family: var(--font-mono); text-transform: uppercase; color: var(--p-accent); font-weight: 600; margin-bottom: 8px;">Active Transfers (' + activeJobs.length + ' / ' + (qStatus.max_concurrent || 2) + ' slots)</div>';
        html += activeJobs.map(j => {
          const cover = j.cover_url || '';
          const title = j.album_title || ('Album #' + j.album_id);
          const artist = j.artist_name || 'Unknown Artist';
          const pct = Math.min(100, Math.max(0, Math.round(j.progress || 0)));
          const statusText = (j.status || 'downloading').toUpperCase();
          const startedAt = j.created_at ? new Date(j.created_at).toLocaleTimeString() : '';

          return '<div class="job-card">' +
            '<img src="' + cover + '" style="width: 48px; height: 48px; border-radius: 6px; object-fit: cover; background: var(--p-surface-0); border: 1px solid var(--p-border); flex-shrink: 0;">' +
            '<div style="flex: 1; min-width: 0;">' +
              '<div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px;">' +
                '<div style="font-weight: 600; font-size: 14.5px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;">' + title + '</div>' +
                '<div style="display: flex; align-items: center; gap: 8px;">' +
                  '<span style="font-family: var(--font-mono); font-size: 11px; background: var(--p-accent-subtle); color: var(--p-accent); border: 1px solid var(--p-accent); padding: 2px 7px; border-radius: 4px;">' + statusText + ' ' + pct + '%</span>' +
                  '<button class="btn btn-secondary btn-sm" style="padding: 2px 8px; font-size: 11px; color: var(--p-danger);" onclick="cancelQueueDownload(' + j.album_id + ')">Cancel</button>' +
                '</div>' +
              '</div>' +
              '<div style="font-size: 12.5px; color: var(--p-muted); font-family: var(--font-mono); margin-bottom: 6px;">' +
                artist + ' • ' + (j.downloader || 'qobuz').toUpperCase() + ' (' + (j.target_quality || 'flac') + ')' +
                (startedAt ? ' • ' + startedAt : '') +
              '</div>' +
              '<div class="progress-bar-container">' +
                '<div class="progress-bar-fill" style="width: ' + pct + '%;"></div>' +
              '</div>' +
            '</div>' +
          '</div>';
        }).join('');
      }

      if (pendingJobs.length > 0) {
        html += '<div style="font-size: 11px; font-family: var(--font-mono); text-transform: uppercase; color: var(--p-muted); font-weight: 600; margin: ' + (activeJobs.length > 0 ? '16px' : '0') + ' 0 8px;">Queued Releases (' + pendingJobs.length + ' waiting)</div>';
        html += pendingJobs.map((j, idx) => {
          const cover = j.cover_url || '';
          const title = j.album_title || ('Album #' + j.album_id);
          const artist = j.artist_name || 'Unknown Artist';

          return '<div class="job-card" style="opacity: 0.85;">' +
            '<img src="' + cover + '" style="width: 44px; height: 44px; border-radius: 6px; object-fit: cover; background: var(--p-surface-0); border: 1px solid var(--p-border); flex-shrink: 0;">' +
            '<div style="flex: 1; min-width: 0;">' +
              '<div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px;">' +
                '<div style="font-weight: 500; font-size: 14px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;">' + title + '</div>' +
                '<div style="display: flex; align-items: center; gap: 8px;">' +
                  '<span style="font-family: var(--font-mono); font-size: 10px; background: var(--p-surface-2); color: var(--p-muted); border: 1px solid var(--p-border); padding: 2px 6px; border-radius: 4px;">QUEUED #' + (idx + 1) + '</span>' +
                  '<button class="btn btn-secondary btn-sm" style="padding: 2px 8px; font-size: 11px; color: var(--p-danger);" onclick="cancelQueueDownload(' + j.album_id + ')">Cancel</button>' +
                '</div>' +
              '</div>' +
              '<div style="font-size: 12px; color: var(--p-muted); font-family: var(--font-mono);">' +
                artist + ' • ' + (j.downloader || 'qobuz').toUpperCase() + ' • Waiting for available download slot' +
              '</div>' +
            '</div>' +
          '</div>';
        }).join('');
      }

      container.innerHTML = html;
    } catch (err) {
      console.error('Queue error:', err);
    }
  }

  async function cancelQueueDownload(albumID) {
    if (!confirm('Cancel this download job?')) return;
    try {
      const res = await fetch('/api/queue/cancel', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ album_id: albumID })
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Failed to cancel');
      showToast('Download cancelled', 'info');
      loadLiveQueue();
    } catch (err) {
      showToast(err.message, 'error');
    }
  }

  function filterHistory(filter, btn) {
    currentHistoryFilter = filter;
    btn.parentElement.querySelectorAll('button').forEach(b => {
      b.classList.remove('active');
      b.style.background = 'transparent';
    });
    btn.classList.add('active');
    btn.style.background = 'var(--p-surface-2)';

    const historyCard = document.getElementById('historyTableCard');
    const releasesCard = document.getElementById('releasesTableCard');

    if (filter === 'releases') {
      historyCard.style.display = 'none';
      releasesCard.style.display = 'block';
      loadActivityReleases();
    } else {
      historyCard.style.display = 'block';
      releasesCard.style.display = 'none';
      loadDownloadHistory();
    }
  }

  async function loadDownloadHistory() {
    const container = document.getElementById('historyTableContainer');
    const countEl = document.getElementById('historyCount');
    if (!container) return;

    try {
      const res = await fetch('/api/history?limit=100');
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || 'HTTP ' + res.status);
      }
      const jobs = await res.json() || [];

      const filtered = jobs.filter(j => {
        if (currentHistoryFilter === 'completed') return j.status === 'completed';
        if (currentHistoryFilter === 'failed') return j.status === 'failed';
        return true;
      });

      if (countEl) countEl.innerText = filtered.length + ' job' + (filtered.length === 1 ? '' : 's');

      if (!filtered.length) {
        container.innerHTML = '<div style="padding: 40px; text-align: center; color: var(--p-muted); font-size: 13px;">' +
          (jobs.length === 0 ? 'No download history recorded yet.' : 'No history matches the selected filter.') + '</div>';
        return;
      }

      let html = '<table>' +
        '<thead>' +
          '<tr>' +
            '<th style="width: 44px;"></th>' +
            '<th>Release</th>' +
            '<th>Artist</th>' +
            '<th>Downloader / Quality</th>' +
            '<th>Date</th>' +
            '<th>Status</th>' +
            '<th style="text-align: right;">Action</th>' +
          '</tr>' +
        '</thead>' +
        '<tbody>';

      html += filtered.map(j => {
        const cover = j.cover_url || '';
        const title = j.album_title || ('Album #' + j.album_id);
        const artist = j.artist_name || '-';
        const isFailed = j.status === 'failed';
        const isCompleted = j.status === 'completed';
        const statusBadge = isCompleted ?
          '<span style="background: var(--p-success-subtle); color: var(--p-success); padding: 3px 8px; border-radius: 4px; font-weight: 500; font-size: 11.5px; font-family: var(--font-mono);">Completed</span>' :
          isFailed ?
          '<span style="background: var(--p-danger-subtle); color: #fca5a5; padding: 3px 8px; border-radius: 4px; font-weight: 500; font-size: 11.5px; font-family: var(--font-mono);">Failed</span>' :
          '<span style="background: var(--p-accent-subtle); color: var(--p-accent); padding: 3px 8px; border-radius: 4px; font-weight: 500; font-size: 11.5px; font-family: var(--font-mono);">' + j.status + '</span>';

        const dateStr = j.created_at ? new Date(j.created_at).toLocaleString() : '-';
        const errBox = (isFailed && j.error_message) ?
          '<div class="error-callout"><strong>Reason:</strong> ' + j.error_message + '</div>' : '';

        return '<tr>' +
          '<td><img src="' + cover + '" style="width: 36px; height: 36px; border-radius: 4px; object-fit: cover; background: var(--p-surface-0);"></td>' +
          '<td><div style="font-weight: 600;">' + title + '</div>' + errBox + '</td>' +
          '<td>' + artist + '</td>' +
          '<td style="font-family: var(--font-mono); font-size: 12px;"><span style="text-transform: uppercase;">' + (j.downloader || 'qobuz') + '</span> • ' + (j.target_quality || '') + '</td>' +
          '<td style="font-family: var(--font-mono); font-size: 12px; color: var(--p-muted);">' + dateStr + '</td>' +
          '<td>' + statusBadge + '</td>' +
          '<td style="text-align: right;">' +
            '<button class="btn btn-secondary btn-sm" onclick="downloadSingleAlbum(\'' + j.album_id + '\', \'' + title.replace(/'/g, "\\'") + '\')">Retry</button>' +
          '</td>' +
        '</tr>';
      }).join('');

      html += '</tbody></table>';
      container.innerHTML = html;
    } catch (err) {
      container.innerHTML = '<div style="color: var(--p-danger); padding: 20px;">History error: ' + err.message + '</div>';
    }
  }

  async function clearDownloadHistory() {
    if (!confirm('Clear all completed and failed download history?')) return;
    try {
      const res = await fetch('/api/history/clear', { method: 'POST' });
      if (!res.ok) throw new Error('Failed to clear history');
      showToast('Download history cleared', 'success');
      loadDownloadHistory();
    } catch (err) {
      showToast('Clear history error: ' + err.message, 'error');
    }
  }

  function filterActivity(status, btn) {
    currentActivityStatus = status;
    btn.parentElement.querySelectorAll('button').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    loadActivityReleases();
  }

  async function loadActivityReleases() {
    const tbody = document.getElementById('activityTableBody');
    if (!tbody) return;
    try {
      const res = await fetch('/api/albums?status=' + currentActivityStatus);
      const albums = await res.json() || [];

      if (!albums.length) {
        tbody.innerHTML = '<tr><td colspan="7" style="text-align: center; padding: 40px; color: var(--p-muted);">No releases match this filter.</td></tr>';
        return;
      }

      tbody.innerHTML = albums.map(alb => {
        const statusClass = alb.status === 'completed' ? 'color: var(--p-success);' : alb.status === 'failed' ? 'color: var(--p-danger);' : alb.status === 'downloading' ? 'color: var(--p-accent);' : 'color: var(--p-muted);';
        return '<tr>' +
          '<td><img src="' + (alb.cover_url || '') + '" style="width: 36px; height: 36px; border-radius: 4px; object-fit: cover; background: var(--p-surface-0);"></td>' +
          '<td style="font-weight: 600;">' + alb.title + '</td>' +
          '<td>' + alb.artist_name + '</td>' +
          '<td><span style="font-family: var(--font-mono); font-size: 10.5px; background: var(--p-surface-0); border: 1px solid var(--p-border); padding: 2px 6px; border-radius: 4px; text-transform: uppercase;">' + (alb.release_type || 'album') + '</span></td>' +
          '<td style="font-family: var(--font-mono);">' + (alb.year || '-') + '</td>' +
          '<td style="' + statusClass + ' font-weight: 600; text-transform: capitalize;">' + alb.status + '</td>' +
          '<td style="text-align: right;">' +
            '<button class="btn btn-secondary btn-sm" onclick="downloadSingleAlbum(\'' + alb.id + '\', \'' + alb.title.replace(/'/g, "\\'") + '\')">' + (alb.status === 'completed' ? 'Re-Download' : 'Download') + '</button>' +
          '</td>' +
        '</tr>';
      }).join('');
    } catch (err) {
      tbody.innerHTML = '<tr><td colspan="7" style="color: var(--p-danger); text-align: center; padding: 20px;">Activity load error: ' + err.message + '</td></tr>';
    }
  }

  // Poll live queue every 2.5 seconds
  setInterval(() => {
    if (currentView === 'activity') {
      loadLiveQueue();
    }
  }, 2500);

  // --- VIEW 4: Settings ---
  async function loadSettings() {
    try {
      const res = await fetch('/api/config');
      currentConfig = await res.json();
      populateSettingsForm(currentConfig);
      loadDisksOverview();
    } catch (err) {
      showToast('Failed to load settings: ' + err.message, 'error');
    }
  }

  function populateSettingsForm(cfg) {
    if (!cfg) return;
    document.getElementById('cfg_music_path').value = cfg.library?.music_path || '';
    document.getElementById('cfg_download_path').value = cfg.library?.download_path || '';
    document.getElementById('cfg_folder_format').value = cfg.library?.folder_format || '{Artist}/{Album} ({Year})';
    document.getElementById('cfg_file_format').value = cfg.library?.file_format || '{Track:02d} - {Title}';

    document.getElementById('cfg_duration_tol').value = cfg.verifier?.duration_tolerance_sec || 4;
    document.getElementById('cfg_min_score').value = cfg.verifier?.min_confidence_score || 0.85;
    document.getElementById('cfg_allow_partial').checked = !!cfg.verifier?.allow_partial_match;

    document.getElementById('cfg_delete_after_import').checked = cfg.import?.delete_after_import !== false;
    document.getElementById('cfg_move_or_copy').value = cfg.import?.move_or_copy || 'move';
    document.getElementById('cfg_clean_empty_folders').checked = cfg.import?.clean_empty_folders !== false;

    document.getElementById('cfg_download_cover_art').checked = cfg.export?.download_cover_art !== false;
    document.getElementById('cfg_cover_filename').value = cfg.export?.cover_filename || 'cover.jpg';
    document.getElementById('cfg_write_nfo').checked = !!cfg.export?.write_nfo;
    document.getElementById('cfg_generate_m3u').checked = !!cfg.export?.generate_m3u;

    document.getElementById('cfg_indexer_provider').value = cfg.indexer?.default_provider || 'deezer';
    document.getElementById('cfg_spotify_client_id').value = cfg.indexer?.spotify?.client_id || '';
    document.getElementById('cfg_spotify_client_secret').value = cfg.indexer?.spotify?.client_secret || '';

    document.getElementById('cfg_preferred_downloader').value = cfg.downloader?.preferred || 'slskd';
    document.getElementById('cfg_max_concurrent_downloads').value = cfg.downloader?.max_concurrent_downloads || 2;
    document.getElementById('cfg_qobuz_dl_enabled').checked = !!cfg.downloader?.qobuz?.enabled;
    document.getElementById('cfg_qobuz_target_quality').value = cfg.downloader?.qobuz?.target_quality || 'flac_16_44';
    document.getElementById('cfg_qobuz_user_token').value = cfg.downloader?.qobuz?.user_token || '';
    document.getElementById('cfg_qobuz_app_id').value = cfg.downloader?.qobuz?.app_id || '798273057';
    document.getElementById('cfg_qobuz_app_secret').value = cfg.downloader?.qobuz?.app_secret || '';

    document.getElementById('cfg_slskd_enabled').checked = !!cfg.downloader?.slskd?.enabled;
    document.getElementById('cfg_slskd_url').value = cfg.downloader?.slskd?.url || 'http://localhost:5030';
    document.getElementById('cfg_slskd_api_key').value = cfg.downloader?.slskd?.api_key || '';
    document.getElementById('cfg_slskd_dir').value = cfg.downloader?.slskd?.download_dir || '';

    document.getElementById('cfg_navi_enabled').checked = !!cfg.navidrome?.enabled;
    document.getElementById('cfg_navi_url').value = cfg.navidrome?.url || 'http://localhost:4533';
    document.getElementById('cfg_navi_username').value = cfg.navidrome?.username || '';
    document.getElementById('cfg_navi_password').value = cfg.navidrome?.password || '';
    document.getElementById('cfg_navi_token_auth').checked = cfg.navidrome?.token_auth !== false;
  }

  async function saveConfiguration(event) {
    if (event) event.preventDefault();

    const newCfg = {
      server: currentConfig?.server || { host: "0.0.0.0", port: 8686 },
      database: currentConfig?.database || { path: "findr.db" },
      library: {
        music_path: document.getElementById('cfg_music_path').value,
        download_path: document.getElementById('cfg_download_path').value,
        folder_format: document.getElementById('cfg_folder_format').value,
        file_format: document.getElementById('cfg_file_format').value
      },
      import: {
        delete_after_import: document.getElementById('cfg_delete_after_import').checked,
        move_or_copy: document.getElementById('cfg_move_or_copy').value,
        clean_empty_folders: document.getElementById('cfg_clean_empty_folders').checked
      },
      export: {
        download_cover_art: document.getElementById('cfg_download_cover_art').checked,
        cover_filename: document.getElementById('cfg_cover_filename').value,
        write_nfo: document.getElementById('cfg_write_nfo').checked,
        generate_m3u: document.getElementById('cfg_generate_m3u').checked
      },
      verifier: {
        duration_tolerance_sec: parseInt(document.getElementById('cfg_duration_tol').value, 10) || 4,
        min_confidence_score: parseFloat(document.getElementById('cfg_min_score').value) || 0.85,
        allow_partial_match: document.getElementById('cfg_allow_partial').checked
      },
      indexer: {
        default_provider: document.getElementById('cfg_indexer_provider').value,
        spotify: {
          client_id: document.getElementById('cfg_spotify_client_id').value,
          client_secret: document.getElementById('cfg_spotify_client_secret').value
        },
        qobuz: {
          app_id: document.getElementById('cfg_qobuz_app_id').value,
          app_secret: document.getElementById('cfg_qobuz_app_secret').value,
          user_token: document.getElementById('cfg_qobuz_user_token').value
        }
      },
      downloader: {
        preferred: document.getElementById('cfg_preferred_downloader').value || 'slskd',
        max_concurrent_downloads: parseInt(document.getElementById('cfg_max_concurrent_downloads').value, 10) || 2,
        qobuz: {
          enabled: document.getElementById('cfg_qobuz_dl_enabled').checked,
          target_quality: document.getElementById('cfg_qobuz_target_quality').value,
          user_token: document.getElementById('cfg_qobuz_user_token').value,
          app_id: document.getElementById('cfg_qobuz_app_id').value,
          app_secret: document.getElementById('cfg_qobuz_app_secret').value,
          username: currentConfig?.downloader?.qobuz?.username || "",
          password: currentConfig?.downloader?.qobuz?.password || ""
        },
        slskd: {
          enabled: document.getElementById('cfg_slskd_enabled').checked,
          url: document.getElementById('cfg_slskd_url').value,
          api_key: document.getElementById('cfg_slskd_api_key').value,
          download_dir: document.getElementById('cfg_slskd_dir').value
        }
      },
      navidrome: {
        enabled: document.getElementById('cfg_navi_enabled').checked,
        url: document.getElementById('cfg_navi_url').value,
        username: document.getElementById('cfg_navi_username').value,
        password: document.getElementById('cfg_navi_password').value,
        token_auth: document.getElementById('cfg_navi_token_auth').checked
      }
    };

    try {
      const res = await fetch('/api/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(newCfg)
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || 'Save failed');
      currentConfig = data.config;
      showToast('Settings saved and live-reloaded!', 'success');
      loadDisksOverview();
    } catch (err) {
      showToast('Save failed: ' + err.message, 'error');
    }
  }

  async function testNavidromeConnection() {
    const resEl = document.getElementById('navidromeTestResult');
    resEl.innerHTML = '<span style="color: var(--p-accent);">Testing Subsonic API ping...</span>';

    const payload = {
      url: document.getElementById('cfg_navi_url').value,
      username: document.getElementById('cfg_navi_username').value,
      password: document.getElementById('cfg_navi_password').value,
      token_auth: document.getElementById('cfg_navi_token_auth').checked
    };

    try {
      const res = await fetch('/api/config/test-navidrome', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      if (data.success) {
        resEl.innerHTML = '<span style="color: var(--p-success); font-weight: 500;">✓ Connected to Navidrome server (Subsonic ping ok)</span>';
      } else {
        resEl.innerHTML = '<span style="color: var(--p-danger);">✕ Connection failed: ' + (data.error || 'Server error') + '</span>';
      }
    } catch (err) {
      resEl.innerHTML = '<span style="color: var(--p-danger);">✕ Request error: ' + err.message + '</span>';
    }
  }

  async function testSlskdConnection() {
    const resEl = document.getElementById('slskdTestResult');
    resEl.innerHTML = '<span style="color: var(--p-accent);">Testing slskd daemon ping...</span>';

    const payload = {
      url: document.getElementById('cfg_slskd_url').value,
      api_key: document.getElementById('cfg_slskd_api_key').value
    };

    try {
      const res = await fetch('/api/config/test-slskd', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      if (data.success) {
        resEl.innerHTML = '<span style="color: var(--p-success); font-weight: 500;">✓ Connected to slskd daemon</span>';
      } else {
        resEl.innerHTML = '<span style="color: var(--p-danger);">✕ Connection failed: ' + (data.error || 'Server error') + '</span>';
      }
    } catch (err) {
      resEl.innerHTML = '<span style="color: var(--p-danger);">✕ Request error: ' + err.message + '</span>';
    }
  }

  async function loadDisksOverview() {
    try {
      const res = await fetch('/api/disks');
      const disks = await res.json() || [];
      renderDisksHtml('systemDisksList', disks);
    } catch (err) {
      console.error(err);
    }
  }

  function renderDisksHtml(elementID, disks) {
    const el = document.getElementById(elementID);
    if (!el) return;
    if (!disks.length) {
      el.innerHTML = '<div style="color: var(--p-muted);">No disks reported.</div>';
      return;
    }

    el.innerHTML = disks.map(d => {
      const pct = Math.round(d.percent_used || 0);
      const fillClass = pct > 90 ? 'danger' : pct > 75 ? 'warning' : '';
      return '<div style="margin-bottom: 14px; background: var(--p-surface-0); padding: 14px 18px; border-radius: var(--r-sm); border: 1px solid var(--p-border);">' +
        '<div style="display: flex; justify-content: space-between; font-size: 13.5px; font-weight: 500;">' +
          '<span style="font-family: var(--font-mono);">' + d.path + '</span>' +
          '<span style="font-family: var(--font-mono); color: var(--p-muted);">' + (d.free_human || '0 B') + ' free of ' + (d.total_human || '0 B') + ' (' + pct + '% used)</span>' +
        '</div>' +
        '<div class="disk-bar-container">' +
          '<div class="disk-bar-fill ' + fillClass + '" style="width: ' + pct + '%;"></div>' +
        '</div>' +
      '</div>';
    }).join('');
  }

  async function loadRawYaml() {
    try {
      const res = await fetch('/api/config/raw');
      const text = await res.text();
      document.getElementById('rawYamlEditor').value = text;
    } catch (err) {
      showToast('Error loading raw yaml: ' + err.message, 'error');
    }
  }

  async function saveRawYaml() {
    const text = document.getElementById('rawYamlEditor').value;
    try {
      const res = await fetch('/api/config/raw', {
        method: 'POST',
        headers: { 'Content-Type': 'text/yaml' },
        body: text
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || 'Error saving YAML');
      showToast('YAML configuration saved!', 'success');
      loadSettings();
    } catch (err) {
      showToast('YAML save failed: ' + err.message, 'error');
    }
  }

  function exportConfigFile() {
    const text = document.getElementById('rawYamlEditor').value;
    const blob = new Blob([text], { type: 'text/yaml' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'config.yaml';
    a.click();
    URL.revokeObjectURL(url);
  }

  function handleYamlFileUpload(event) {
    const file = event.target.files[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = function(e) {
      document.getElementById('rawYamlEditor').value = e.target.result;
      showToast('Loaded ' + file.name + ' into editor. Click Save to apply.', 'info');
    };
    reader.readAsText(file);
  }

  async function loadSystemOverview() {
    try {
      const res = await fetch('/api/status');
      const data = await res.json();
      document.getElementById('sysUptime').innerText = formatUptime(data.uptime_seconds);
      document.getElementById('sysArtistCount').innerText = data.monitored_artists || 0;
      document.getElementById('sysAlbumCount').innerText = data.monitored_albums || 0;
      document.getElementById('sysDownloader').innerText = data.downloader || 'qobuz';
      document.getElementById('sysNavidrome').innerText = data.navidrome_enabled ? 'Enabled' : 'Disabled';
      document.getElementById('sysIndexer').innerText = currentConfig?.indexer?.default_provider || 'deezer';
      loadDisksOverview();
    } catch (err) {
      console.error(err);
    }
  }

  function formatUptime(sec) {
    if (!sec) return '0s';
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    let res = '';
    if (d > 0) res += d + 'd ';
    if (h > 0) res += h + 'h ';
    if (m > 0) res += m + 'm ';
    res += s + 's';
    return res;
  }

  async function triggerNavidromeScan() {
    const scanIcon = document.getElementById('scanIcon');
    if (scanIcon) scanIcon.classList.add('scan-spin');
    showToast('Triggering Navidrome library scan...', 'info');

    try {
      const res = await fetch('/api/scan', { method: 'POST' });
      let data = {};
      try {
        data = await res.json();
      } catch (_) {
        const text = await res.text().catch(() => '');
        data = { message: text };
      }
      if (res.ok) {
        showToast('Navidrome library scan initiated', 'success');
      } else {
        showToast('Scan error: ' + (data.message || data.error || 'Navidrome not enabled'), 'error');
      }
    } catch (err) {
      showToast('Scan error: ' + err.message, 'error');
    } finally {
      setTimeout(() => {
        if (scanIcon) scanIcon.classList.remove('scan-spin');
      }, 1000);
    }
  }

  // --- VIEW: Manual Import & Upload ---
  let detectedImportFiles = [];
  let importArtistsList = [];

  async function initManualImport() {
    switchImportMode('upload');
    await populateImportArtistOptions();
  }

  function switchImportMode(mode) {
    const btnUpload = document.getElementById('btnModeUpload');
    const btnScan = document.getElementById('btnModeScan');
    const paneUpload = document.getElementById('paneUpload');
    const paneScan = document.getElementById('paneScan');

    if (mode === 'scan') {
      if (btnUpload) btnUpload.classList.remove('active');
      if (btnScan) btnScan.classList.add('active');
      if (paneUpload) paneUpload.classList.remove('active');
      if (paneScan) paneScan.classList.add('active');
    } else {
      if (btnUpload) btnUpload.classList.add('active');
      if (btnScan) btnScan.classList.remove('active');
      if (paneUpload) paneUpload.classList.add('active');
      if (paneScan) paneScan.classList.remove('active');
    }
  }

  function handleDragOver(e) {
    e.preventDefault();
    e.stopPropagation();
    const dz = document.getElementById('audioDropzone');
    if (dz) dz.classList.add('drag-over');
  }

  function handleDragLeave(e) {
    e.preventDefault();
    e.stopPropagation();
    const dz = document.getElementById('audioDropzone');
    if (dz) dz.classList.remove('drag-over');
  }

  function handleFileDrop(e) {
    e.preventDefault();
    e.stopPropagation();
    const dz = document.getElementById('audioDropzone');
    if (dz) dz.classList.remove('drag-over');
    const files = e.dataTransfer?.files;
    if (files && files.length > 0) {
      uploadAudioFiles(files);
    }
  }

  function handleAudioFileSelect(e) {
    const files = e.target.files;
    if (files && files.length > 0) {
      uploadAudioFiles(files);
    }
  }

  function uploadAudioFiles(files) {
    const progressContainer = document.getElementById('uploadProgressContainer');
    const statusText = document.getElementById('uploadStatusText');
    const percentText = document.getElementById('uploadPercentText');
    const progressBar = document.getElementById('uploadProgressBar');

    if (progressContainer) progressContainer.style.display = 'block';
    if (statusText) statusText.innerText = 'Uploading ' + files.length + ' file(s)...';
    if (percentText) percentText.innerText = '0%';
    if (progressBar) progressBar.style.width = '0%';

    const formData = new FormData();
    for (let i = 0; i < files.length; i++) {
      formData.append('files', files[i]);
    }

    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/import/upload', true);

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        if (percentText) percentText.innerText = pct + '%';
        if (progressBar) progressBar.style.width = pct + '%';
      }
    };

    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          const res = JSON.parse(xhr.responseText);
          detectedImportFiles = res.files || [];
          showToast('Uploaded and parsed ' + detectedImportFiles.length + ' audio files', 'success');
          renderDetectedFiles();
          autoFillImportDestination();
        } catch (err) {
          showToast('Failed to parse upload response', 'error');
        }
      } else {
        showToast('Upload failed (HTTP ' + xhr.status + ')', 'error');
      }
      setTimeout(() => {
        if (progressContainer) progressContainer.style.display = 'none';
      }, 1500);
    };

    xhr.onerror = () => {
      showToast('Network error during upload', 'error');
      if (progressContainer) progressContainer.style.display = 'none';
    };

    xhr.send(formData);
  }

  async function executeDirectoryScan() {
    const input = document.getElementById('importScanPathInput');
    const path = input ? input.value.trim() : '';
    if (!path) {
      showToast('Please specify a directory path to scan', 'info');
      return;
    }

    showToast('Scanning directory: ' + path + '...', 'info');
    try {
      const res = await fetch('/api/import/scan', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path: path })
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Failed to scan directory');
      detectedImportFiles = data.files || [];
      showToast('Found and cataloged ' + detectedImportFiles.length + ' audio files', 'success');
      renderDetectedFiles();
      autoFillImportDestination();
    } catch (err) {
      showToast(err.message, 'error');
    }
  }

  function renderDetectedFiles() {
    const card = document.getElementById('importReviewCard');
    const container = document.getElementById('detectedFilesTableContainer');
    if (!card || !container) return;

    if (!detectedImportFiles || detectedImportFiles.length === 0) {
      card.style.display = 'none';
      container.innerHTML = '';
      return;
    }

    card.style.display = 'block';

    let html = '<table><thead><tr>' +
      '<th style="width: 44px;">#</th>' +
      '<th>Title</th>' +
      '<th>Artist</th>' +
      '<th>Album</th>' +
      '<th>Duration</th>' +
      '<th>Size</th>' +
      '<th>Source File</th>' +
    '</tr></thead><tbody>';

    html += detectedImportFiles.map((f, idx) => {
      const trackNum = f.track_number ? (f.track_number < 10 ? '0' + f.track_number : f.track_number) : (idx + 1);
      const title = f.title || f.file_name || 'Track ' + trackNum;
      const artist = f.artist || '-';
      const album = f.album || '-';
      const dur = formatDuration(f.duration_sec);
      const sizeMB = (f.size_bytes / (1024 * 1024)).toFixed(1) + ' MB';
      const fileName = f.file_name || '';

      return '<tr>' +
        '<td style="font-family: var(--font-mono); color: var(--p-muted);">' + trackNum + '</td>' +
        '<td style="font-weight: 500;">' + title + '</td>' +
        '<td>' + artist + '</td>' +
        '<td>' + album + '</td>' +
        '<td style="font-family: var(--font-mono); font-size: 12px; color: var(--p-muted);">' + dur + '</td>' +
        '<td style="font-family: var(--font-mono); font-size: 12px; color: var(--p-muted);">' + sizeMB + '</td>' +
        '<td style="font-family: var(--font-mono); font-size: 11px; color: var(--p-muted); max-width: 180px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;" title="' + f.file_path + '">' + fileName + '</td>' +
      '</tr>';
    }).join('');

    html += '</tbody></table>';
    container.innerHTML = html;
  }

  async function populateImportArtistOptions() {
    try {
      const res = await fetch('/api/artists');
      importArtistsList = await res.json() || [];
    } catch (e) {
      importArtistsList = [];
    }

    const select = document.getElementById('importArtistSelect');
    if (!select) return;

    let html = '<option value="">-- Choose Existing Artist or Create New --</option>';
    importArtistsList.forEach(a => {
      html += '<option value="' + a.id + '">' + a.name + '</option>';
    });
    html += '<option value="__custom__">+ Create New Artist...</option>';
    select.innerHTML = html;
  }

  async function handleImportArtistChange() {
    const artistSelect = document.getElementById('importArtistSelect');
    const newArtistInput = document.getElementById('importNewArtistInput');
    const albumSelect = document.getElementById('importAlbumSelect');
    const newAlbumInput = document.getElementById('importNewAlbumInput');

    if (!artistSelect) return;
    const val = artistSelect.value;

    if (val === '__custom__') {
      if (newArtistInput) newArtistInput.style.display = 'block';
      if (albumSelect) {
        albumSelect.innerHTML = '<option value="__custom__">+ Create New Album...</option>';
      }
      if (newAlbumInput) newAlbumInput.style.display = 'block';
      return;
    }

    if (newArtistInput) newArtistInput.style.display = 'none';

    if (!val) {
      if (albumSelect) albumSelect.innerHTML = '<option value="">-- Select an Artist First --</option>';
      if (newAlbumInput) newAlbumInput.style.display = 'none';
      return;
    }

    // Load releases for selected artist
    try {
      const res = await fetch('/api/artists/' + val);
      const artistData = await res.json();
      const releases = artistData.releases || [];

      let albHtml = '<option value="">-- Select an Existing Album --</option>';
      releases.forEach(r => {
        albHtml += '<option value="' + r.id + '" data-type="' + (r.type || 'album') + '" data-year="' + (r.year || '') + '">' +
          r.title + (r.year ? ' (' + r.year + ')' : '') + ' [' + (r.type || 'album').toUpperCase() + ']' +
        '</option>';
      });
      albHtml += '<option value="__custom__">+ Create New Album...</option>';
      if (albumSelect) albumSelect.innerHTML = albHtml;
      if (newAlbumInput) newAlbumInput.style.display = 'none';
    } catch (err) {
      console.error('Failed to load artist releases', err);
    }
  }

  function handleImportAlbumChange() {
    const albumSelect = document.getElementById('importAlbumSelect');
    const newAlbumInput = document.getElementById('importNewAlbumInput');
    const releaseType = document.getElementById('importReleaseType');
    const yearInput = document.getElementById('importYearInput');

    if (!albumSelect) return;
    const val = albumSelect.value;

    if (val === '__custom__') {
      if (newAlbumInput) newAlbumInput.style.display = 'block';
    } else {
      if (newAlbumInput) newAlbumInput.style.display = 'none';
      const opt = albumSelect.selectedOptions[0];
      if (opt) {
        const type = opt.getAttribute('data-type');
        const year = opt.getAttribute('data-year');
        if (type && releaseType) releaseType.value = type;
        if (year && yearInput) yearInput.value = year;
      }
    }
  }

  function autoFillImportDestination() {
    if (!detectedImportFiles || detectedImportFiles.length === 0) return;

    const first = detectedImportFiles[0];
    const artistName = (first.artist || '').trim();
    const albumTitle = (first.album || '').trim();

    const artistSelect = document.getElementById('importArtistSelect');
    const newArtistInput = document.getElementById('importNewArtistInput');
    const albumSelect = document.getElementById('importAlbumSelect');
    const newAlbumInput = document.getElementById('importNewAlbumInput');

    if (artistName && artistSelect) {
      const match = importArtistsList.find(a => a.name.toLowerCase() === artistName.toLowerCase());
      if (match) {
        artistSelect.value = match.id;
        handleImportArtistChange().then(() => {
          if (albumTitle && albumSelect) {
            let found = false;
            for (let i = 0; i < albumSelect.options.length; i++) {
              if (albumSelect.options[i].text.toLowerCase().includes(albumTitle.toLowerCase())) {
                albumSelect.selectedIndex = i;
                handleImportAlbumChange();
                found = true;
                break;
              }
            }
            if (!found) {
              albumSelect.value = '__custom__';
              if (newAlbumInput) {
                newAlbumInput.style.display = 'block';
                newAlbumInput.value = albumTitle;
              }
            }
          }
        });
      } else {
        artistSelect.value = '__custom__';
        if (newArtistInput) {
          newArtistInput.style.display = 'block';
          newArtistInput.value = artistName;
        }
        if (albumSelect) {
          albumSelect.innerHTML = '<option value="__custom__">+ Create New Album...</option>';
          albumSelect.value = '__custom__';
        }
        if (newAlbumInput) {
          newAlbumInput.style.display = 'block';
          newAlbumInput.value = albumTitle;
        }
      }
    }
  }

  async function executeManualImport() {
    if (!detectedImportFiles || detectedImportFiles.length === 0) {
      showToast('No audio files to import', 'info');
      return;
    }

    const artistSelect = document.getElementById('importArtistSelect');
    const newArtistInput = document.getElementById('importNewArtistInput');
    const albumSelect = document.getElementById('importAlbumSelect');
    const newAlbumInput = document.getElementById('importNewAlbumInput');
    const releaseType = document.getElementById('importReleaseType');
    const yearInput = document.getElementById('importYearInput');
    const moveOrCopy = document.getElementById('importMoveOrCopy');

    let artistID = 0;
    let artistName = '';
    if (artistSelect.value === '__custom__') {
      artistName = newArtistInput.value.trim();
    } else if (artistSelect.value) {
      artistID = parseInt(artistSelect.value, 10);
      const found = importArtistsList.find(a => a.id === artistID);
      if (found) artistName = found.name;
    }

    let albumID = 0;
    let albumTitle = '';
    if (albumSelect.value === '__custom__') {
      albumTitle = newAlbumInput.value.trim();
    } else if (albumSelect.value) {
      albumID = parseInt(albumSelect.value, 10);
      const opt = albumSelect.selectedOptions[0];
      if (opt) albumTitle = opt.text.split(' [')[0];
    }

    if (!artistName) {
      showToast('Please specify or select an artist', 'error');
      return;
    }
    if (!albumTitle) {
      showToast('Please specify or select an album', 'error');
      return;
    }

    const payload = {
      artist_id: artistID,
      artist_name: artistName,
      album_id: albumID,
      album_title: albumTitle,
      release_type: releaseType ? releaseType.value : 'album',
      year: yearInput ? (parseInt(yearInput.value, 10) || 0) : 0,
      move_or_copy: moveOrCopy ? moveOrCopy.value : 'move',
      files: detectedImportFiles
    };

    showToast('Importing ' + detectedImportFiles.length + ' tracks into library...', 'info');
    try {
      const res = await fetch('/api/import/execute', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Import failed');

      showToast('Successfully imported to: ' + data.destination, 'success');
      detectedImportFiles = [];
      renderDetectedFiles();
      loadLibraryArtists();
      setTimeout(() => navigateTo('artists'), 1200);
    } catch (err) {
      showToast(err.message, 'error');
    }
  }

  // Router init & hash routing
  function handleRoute() {
    let hash = window.location.hash.replace(/^#/, '');
    if (!hash) {
      const path = window.location.pathname;
      if (path.startsWith('/artist/')) {
        hash = 'artist/' + path.replace('/artist/', '');
      } else if (path.startsWith('/search')) {
        hash = 'search';
      } else if (path.startsWith('/activity') || path.startsWith('/queue')) {
        hash = 'activity';
      } else if (path.startsWith('/import')) {
        hash = 'import';
      } else if (path.startsWith('/settings')) {
        hash = 'settings';
      } else if (path.startsWith('/system')) {
        hash = 'system';
      }
    }

    if (hash.startsWith('artist/preview-')) {
      // preview state handled by search preview interaction
      return;
    }
    if (hash.startsWith('artist/')) {
      const id = hash.replace('artist/', '');
      if (id) {
        openArtistPage(decodeURIComponent(id), false);
        return;
      }
    }
    if (['artists', 'search', 'activity', 'import', 'settings', 'system'].includes(hash)) {
      navigateTo(hash, false);
    } else {
      navigateTo('artists', false);
    }
  }

  window.addEventListener('DOMContentLoaded', handleRoute);
  window.addEventListener('hashchange', handleRoute);
</script>

</body>
</html>`
