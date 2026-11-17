package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/findr-app/findr/internal/files"
)

// Handlers for single-album tracking, library file management and playlist import.

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error(), "message": err.Error()})
}

// ---- albums ----

func (s *Server) handleSearchAlbums(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	results, err := s.orch.SearchAlbums(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

// handleAddAlbum serves POST /api/albums: track a single release.
func (s *Server) handleAddAlbum(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID string `json:"source_id"`
		ID       string `json:"id"`
		Download bool   `json:"download"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	id := req.SourceID
	if id == "" {
		id = req.ID
	}
	album, err := s.orch.AddAlbum(r.Context(), id, req.Download)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	msg := fmt.Sprintf("Tracking %s by %s", album.Title, album.ArtistName)
	if req.Download {
		msg = fmt.Sprintf("Tracking and queued %s by %s", album.Title, album.ArtistName)
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"album": album, "message": msg})
}

// handleAlbumFiles serves GET/DELETE /api/albums/{id}/files.
func (s *Server) handleAlbumFiles(w http.ResponseWriter, r *http.Request, albumID string) {
	ctx := r.Context()
	album, dir, err := s.orch.LocateAlbumDir(ctx, albumID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	fm := s.fileManager()

	resp := map[string]interface{}{"album_id": album.ID, "root": "music", "exists": false}
	rel := ""
	if dir != "" {
		var ok bool
		rel, ok = fm.RelPath("music", dir)
		if !ok {
			dir = "" // outside the library root: do not expose or touch it
		}
	}

	switch r.Method {
	case http.MethodGet:
		if dir == "" {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		listing, err := fm.List("music", rel)
		if err != nil {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp["exists"] = true
		resp["path"] = rel
		resp["listing"] = listing
		writeJSON(w, http.StatusOK, resp)

	case http.MethodDelete:
		if dir == "" || rel == "" {
			writeErr(w, http.StatusNotFound, errors.New("no folder for this release was found in the library"))
			return
		}
		if err := fm.Delete("music", rel); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		fm.RemoveEmptyParents("music", dir)
		unmonitor := r.URL.Query().Get("unmonitor") == "true"
		updated, err := s.orch.MarkAlbumFilesRemoved(ctx, albumID, unmonitor)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"album":   updated,
			"message": fmt.Sprintf("Deleted %s from disk", rel),
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- files ----

func (s *Server) fileManager() *files.Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return files.NewManager(
		files.Root{Name: "music", Label: "Music library", Path: s.cfg.Library.MusicPath},
		files.Root{Name: "downloads", Label: "Download staging", Path: s.cfg.Library.DownloadPath},
	)
}

func fileErrStatus(err error) int {
	switch {
	case errors.Is(err, files.ErrOutsideRoot):
		return http.StatusForbidden
	case errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) handleFileRoots(w http.ResponseWriter, r *http.Request) {
	type rootInfo struct {
		files.Root
		Exists bool `json:"exists"`
	}
	var out []rootInfo
	for _, root := range s.fileManager().Roots() {
		_, err := os.Stat(root.Path)
		out = append(out, rootInfo{Root: root, Exists: err == nil})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFiles serves /api/files:
//
//	GET    ?root=&path=           list a folder
//	DELETE ?root=&path=           delete a file or folder
//	POST   {action, root, path, name, dest}  rename | move | mkdir
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	fm := s.fileManager()
	q := r.URL.Query()

	switch r.Method {
	case http.MethodGet:
		root := q.Get("root")
		if root == "" {
			root = "music"
		}
		listing, err := fm.List(root, q.Get("path"))
		if err != nil {
			writeErr(w, fileErrStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, listing)

	case http.MethodDelete:
		root, rel := q.Get("root"), q.Get("path")
		abs, err := fm.Resolve(root, rel)
		if err != nil {
			writeErr(w, fileErrStatus(err), err)
			return
		}
		info, statErr := os.Stat(abs)
		if err := fm.Delete(root, rel); err != nil {
			writeErr(w, fileErrStatus(err), err)
			return
		}
		marked := 0
		if root == "music" && statErr == nil && info.IsDir() {
			marked = s.orch.MarkDeletedPath(r.Context(), abs)
		}
		msg := "Deleted " + filepath.Base(abs)
		if marked > 0 {
			msg += fmt.Sprintf("; %d releases marked missing", marked)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"message": msg, "releases_reset": marked})

	case http.MethodPost:
		var req struct {
			Action string `json:"action"`
			Root   string `json:"root"`
			Path   string `json:"path"`
			Name   string `json:"name"`
			Dest   string `json:"dest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("invalid request body"))
			return
		}
		var newPath string
		var err error
		switch req.Action {
		case "rename":
			newPath, err = fm.Rename(req.Root, req.Path, req.Name)
		case "move":
			newPath, err = fm.Move(req.Root, req.Path, req.Dest)
		case "mkdir":
			newPath, err = fm.Mkdir(req.Root, req.Path, req.Name)
		default:
			err = fmt.Errorf("unknown action %q", req.Action)
		}
		if err != nil {
			writeErr(w, fileErrStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": newPath, "message": "Done"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFileRaw streams a single file (audio preview, cover art, download).
func (s *Server) handleFileRaw(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	abs, err := s.fileManager().Resolve(q.Get("root"), q.Get("path"))
	if err != nil {
		writeErr(w, fileErrStatus(err), err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeErr(w, fileErrStatus(err), err)
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, errors.New("cannot download a folder"))
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	if q.Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(abs)))
	}
	http.ServeContent(w, r, filepath.Base(abs), info.ModTime(), f)
}

// ---- playlists ----

func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		pls, err := s.orch.ListPlaylistSummaries(ctx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, pls)

	case http.MethodPost:
		var req struct {
			URL      string    `json:"url"`
			AlbumIDs *[]string `json:"album_ids"`
			Download bool      `json:"download"`
			Monitor  *bool     `json:"monitor"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("invalid request body"))
			return
		}
		monitor := true
		if req.Monitor != nil {
			monitor = *req.Monitor
		}
		var albumIDs []string
		if req.AlbumIDs != nil {
			albumIDs = *req.AlbumIDs
			if albumIDs == nil {
				albumIDs = []string{}
			}
		}
		rctx, cancel := context.WithTimeout(ctx, 55*time.Second)
		defer cancel()
		res, err := s.orch.ImportPlaylist(rctx, req.URL, albumIDs, req.Download, monitor)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePlaylistPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	// The server's write timeout is 60 s; leave room to send the response.
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	preview, err := s.orch.PreviewPlaylist(ctx, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// handlePlaylistDetail serves /api/playlists/{id}[/sync|/download|/m3u].
func (s *Server) handlePlaylistDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/playlists/"), "/")
	id := parts[0]
	if id == "preview" {
		s.handlePlaylistPreview(w, r)
		return
	}
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing playlist id"))
		return
	}

	if len(parts) == 2 && r.Method == http.MethodPost {
		switch parts[1] {
		case "sync":
			download := r.URL.Query().Get("download") == "true"
			go func() {
				if _, err := s.orch.SyncPlaylist(context.Background(), id, download); err != nil {
					fmt.Printf("[findr Playlist ERROR] sync %s: %v\n", id, err)
				}
			}()
			writeJSON(w, http.StatusAccepted, map[string]string{"message": "Syncing playlist from Spotify"})
		case "download":
			n, err := s.orch.EnqueuePlaylistMissing(ctx, id)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"queued": n, "message": fmt.Sprintf("Queued %d missing releases", n)})
		case "m3u":
			res, err := s.orch.WritePlaylistM3U(ctx, id)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			msg := fmt.Sprintf("Wrote %d of %d tracks to %s", res.Found, res.Found+res.Missing, filepath.Base(res.Path))
			if res.Found == 0 {
				msg = "None of the playlist's tracks are on disk yet, so no file was written"
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"result": res, "message": msg})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		summary, albums, err := s.orch.GetPlaylistDetail(ctx, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"playlist": summary, "albums": albums})

	case http.MethodPut:
		var req struct {
			Monitored *bool `json:"monitored"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("invalid request body"))
			return
		}
		pl, err := s.repo.GetPlaylist(ctx, id)
		if err != nil || pl == nil {
			writeErr(w, http.StatusNotFound, errors.New("playlist not found"))
			return
		}
		if req.Monitored != nil {
			pl.Monitored = *req.Monitored
		}
		if err := s.repo.SavePlaylist(ctx, pl); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, pl)

	case http.MethodDelete:
		if err := s.orch.DeletePlaylist(ctx, id, r.URL.Query().Get("remove_m3u") == "true"); err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Playlist removed"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
