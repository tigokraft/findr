package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/navidrome"
	"github.com/findr-app/findr/internal/orchestrator"
	"github.com/findr-app/findr/internal/storage"
)

// Server provides HTTP REST APIs and Web UI for findr.
type Server struct {
	cfg        *config.Config
	orch       *orchestrator.Orchestrator
	repo       storage.Repository
	navidrome  *navidrome.Client
	httpServer *http.Server
	startTime  time.Time
	mu         sync.Mutex
}

// NewServer initializes a new Server instance.
func NewServer(cfg *config.Config, orch *orchestrator.Orchestrator, repo storage.Repository, navi *navidrome.Client) *Server {
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	s := &Server{
		cfg:       cfg,
		orch:      orch,
		repo:      repo,
		navidrome: navi,
		startTime: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/artists", s.handleArtists)
	mux.HandleFunc("/api/artists/", s.handleArtistDetail)
	mux.HandleFunc("/api/albums/", s.handleAlbumActions)
	mux.HandleFunc("/api/scan", s.handleScan)
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
	// Start background scheduler (checks for new releases every 6 hours)
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

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "findr",
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	artists, _ := s.repo.ListArtists(ctx)
	albums, _ := s.repo.ListMonitoredAlbums(ctx)

	completed := 0
	for _, a := range albums {
		if a.Status == domain.StatusCompleted {
			completed++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":            "running",
		"uptime_seconds":    int(time.Since(s.startTime).Seconds()),
		"monitored_artists": len(artists),
		"monitored_albums":  len(albums),
		"completed_albums":  completed,
		"music_path":        s.cfg.Library.MusicPath,
		"downloader":        s.cfg.Downloader.Preferred,
		"navidrome_enabled": s.cfg.Navidrome.Enabled,
	})
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
			DownloadAll bool   `json:"download_all"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
			http.Error(w, "invalid request body: query required", http.StatusBadRequest)
			return
		}

		if req.DownloadAll {
			go func() {
				bgCtx := context.Background()
				_, _, _ = s.orch.TrackAndDownloadArtist(bgCtx, req.Query, nil, nil)
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

	if len(parts) == 2 && parts[1] == "download-all" && r.Method == http.MethodPost {
		go func() {
			bgCtx := context.Background()
			_, _ = s.orch.DownloadArtistReleases(bgCtx, artistID, nil, nil)
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": fmt.Sprintf("Triggered discography download for artist %s", artistID),
		})
		return
	}

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
}

func (s *Server) handleAlbumActions(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/albums/")
	parts := strings.Split(path, "/")
	albumID := parts[0]

	if len(parts) == 2 && parts[1] == "download" && r.Method == http.MethodPost {
		go func() {
			bgCtx := context.Background()
			_, _, _ = s.orch.ProcessAlbumPipeline(bgCtx, albumID, nil)
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": fmt.Sprintf("Download initiated for album %s", albumID),
		})
		return
	}

	album, err := s.repo.GetAlbum(r.Context(), albumID)
	if err != nil || album == nil {
		http.Error(w, "album not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(album)
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if s.navidrome == nil {
		http.Error(w, "navidrome integration not enabled", http.StatusBadRequest)
		return
	}

	status, err := s.navidrome.StartScan(r.Context(), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	html := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>findr - Music Manager</title>
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <style>
    :root {
      --bg: #0f1117;
      --card: #1a1d27;
      --border: #2a2e3d;
      --text: #f0f2f8;
      --text-muted: #8b92a5;
      --accent: #6366f1;
      --accent-hover: #4f46e5;
      --success: #10b981;
    }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: var(--bg);
      color: var(--text);
      margin: 0;
      padding: 24px;
    }
    .header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      border-bottom: 1px solid var(--border);
      padding-bottom: 16px;
      margin-bottom: 24px;
    }
    h1 { margin: 0; font-size: 24px; font-weight: 700; color: var(--text); }
    .badge {
      background: rgba(99, 102, 241, 0.15);
      color: var(--accent);
      padding: 4px 10px;
      border-radius: 9999px;
      font-size: 13px;
      font-weight: 600;
    }
    .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 16px; }
    .card {
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 12px;
      padding: 18px;
    }
    .search-box {
      display: flex;
      gap: 12px;
      margin-bottom: 24px;
    }
    input[type="text"] {
      flex: 1;
      background: var(--card);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 12px 16px;
      color: var(--text);
      font-size: 15px;
    }
    button {
      background: var(--accent);
      color: white;
      border: none;
      border-radius: 8px;
      padding: 12px 20px;
      font-weight: 600;
      cursor: pointer;
    }
    button:hover { background: var(--accent-hover); }
    .btn-secondary {
      background: var(--border);
      color: var(--text);
    }
    .btn-secondary:hover { background: #3a3f52; }
    .artist-title { font-size: 18px; font-weight: 600; margin-bottom: 8px; }
    .stats { color: var(--text-muted); font-size: 14px; margin-bottom: 14px; }
  </style>
</head>
<body>
  <div class="header">
    <div style="display: flex; align-items: center; gap: 12px;">
      <h1>findr</h1>
      <span class="badge">Go Core • Deezer/Qobuz • Navidrome</span>
    </div>
    <button class="btn-secondary" onclick="triggerScan()">Trigger Navidrome Scan</button>
  </div>

  <div class="search-box">
    <input type="text" id="artistInput" placeholder="Enter artist name (e.g. Daft Punk, Justice)..." onkeydown="if(event.key==='Enter') trackArtist(true)">
    <button onclick="trackArtist(false)">Add & Monitor</button>
    <button style="background: var(--success);" onclick="trackArtist(true)">⚡ Track & Get Everything</button>
  </div>

  <div id="statusAlert" style="margin-bottom: 20px; color: var(--accent); font-weight: 600;"></div>

  <h2>Monitored Artists</h2>
  <div class="grid" id="artistsGrid">Loading artists...</div>

  <script>
    async function loadArtists() {
      try {
        const res = await fetch('/api/artists');
        const artists = await res.json();
        const grid = document.getElementById('artistsGrid');
        if (!artists || artists.length === 0) {
          grid.innerHTML = '<div style="color: var(--text-muted);">No monitored artists yet. Add one above!</div>';
          return;
        }
        grid.innerHTML = artists.map(function(a) {
          return '<div class="card">' +
            '<div class="artist-title">' + a.name + '</div>' +
            '<div class="stats">Source: ' + a.source + ' (ID: ' + a.source_id + ')</div>' +
            '<button style="width: 100%; font-size: 13px;" onclick="downloadAll(\'' + a.id + '\', \'' + a.name + '\')">Download All Releases</button>' +
          '</div>';
        }).join('');
      } catch (err) {
        console.error(err);
      }
    }

    async function trackArtist(downloadAll) {
      const input = document.getElementById('artistInput');
      const query = input.value.trim();
      if (!query) return;

      const alert = document.getElementById('statusAlert');
      alert.innerText = 'Searching and syncing ' + query + '...';

      try {
        const res = await fetch('/api/artists', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ query: query, download_all: downloadAll })
        });
        const data = await res.json();
        alert.innerText = downloadAll ? ('Started downloading all releases for ' + query + '!') : ('Successfully tracked ' + query + '!');
        input.value = '';
        setTimeout(loadArtists, 1000);
      } catch (err) {
        alert.innerText = 'Error: ' + err.message;
      }
    }

    async function downloadAll(artistID, artistName) {
      const alert = document.getElementById('statusAlert');
      alert.innerText = 'Initiated discography download for ' + artistName + '...';
      await fetch('/api/artists/' + artistID + '/download-all', { method: 'POST' });
    }

    async function triggerScan() {
      const alert = document.getElementById('statusAlert');
      alert.innerText = 'Triggering Navidrome scan...';
      try {
        const res = await fetch('/api/scan', { method: 'POST' });
        const data = await res.json();
        alert.innerText = 'Navidrome scan triggered! (Current count: ' + (data.count || 'syncing') + ')';
      } catch (err) {
        alert.innerText = 'Navidrome scan error: ' + err.message;
      }
    }

    loadArtists();
  </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}
