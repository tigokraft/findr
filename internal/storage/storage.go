package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/findr-app/findr/internal/domain"
	_ "modernc.org/sqlite" // Pure Go SQLite driver (no CGO)
)

// Repository defines data access for artists, albums, and download jobs.
type Repository interface {
	SaveArtist(ctx context.Context, artist *domain.Artist) error
	GetArtist(ctx context.Context, id string) (*domain.Artist, error)
	ListArtists(ctx context.Context) ([]domain.Artist, error)
	DeleteArtist(ctx context.Context, id string) error

	SaveAlbum(ctx context.Context, album *domain.Album) error
	GetAlbum(ctx context.Context, id string) (*domain.Album, error)
	ListAlbumsByArtist(ctx context.Context, artistID string) ([]domain.Album, error)
	ListMonitoredAlbums(ctx context.Context) ([]domain.Album, error)
	DeleteAlbum(ctx context.Context, id string) error

	SavePlaylist(ctx context.Context, pl *domain.Playlist) error
	GetPlaylist(ctx context.Context, id string) (*domain.Playlist, error)
	ListPlaylists(ctx context.Context) ([]domain.Playlist, error)
	DeletePlaylist(ctx context.Context, id string) error

	SaveDownloadJob(ctx context.Context, job *domain.DownloadJob) error
	GetDownloadJob(ctx context.Context, id string) (*domain.DownloadJob, error)
	ListActiveDownloadJobs(ctx context.Context) ([]domain.DownloadJob, error)
	ListDownloadJobs(ctx context.Context, limit int) ([]domain.DownloadJob, error)
	ClearDownloadHistory(ctx context.Context) error

	Close() error
}

// SQLiteRepository implements Repository backed by SQLite.
type SQLiteRepository struct {
	db *sql.DB
}

// NewSQLiteRepository initializes SQLite database and runs initial migrations.
func NewSQLiteRepository(path string) (*SQLiteRepository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db at %s: %w", path, err)
	}

	// Limit connection pool to 1 open connection to prevent SQLite busy locks in DELETE journal mode
	db.SetMaxOpenConns(1)

	// Enable busy timeout and synchronous settings for concurrent access.
	// NOTE: We use journal_mode = DELETE instead of WAL because WAL creates shared-memory (.shm)
	// files that fail with disk I/O error (522) on macOS Docker VirtioFS host bind-mounts.
	pragmas := []string{
		"PRAGMA journal_mode = DELETE;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("execute pragma %s: %w", p, err)
		}
	}

	repo := &SQLiteRepository{db: db}
	if err := repo.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return repo, nil
}

func (r *SQLiteRepository) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS artists (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		overview TEXT,
		image_url TEXT,
		source TEXT NOT NULL,
		source_id TEXT NOT NULL,
		monitored INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS albums (
		id TEXT PRIMARY KEY,
		artist_id TEXT NOT NULL,
		artist_name TEXT NOT NULL,
		title TEXT NOT NULL,
		release_type TEXT NOT NULL,
		release_date TEXT,
		year INTEGER,
		cover_url TEXT,
		total_tracks INTEGER,
		upc TEXT,
		source TEXT NOT NULL,
		source_id TEXT NOT NULL,
		monitored INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL,
		tracks_json TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		FOREIGN KEY(artist_id) REFERENCES artists(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS download_jobs (
		id TEXT PRIMARY KEY,
		album_id TEXT NOT NULL,
		downloader TEXT NOT NULL,
		download_dir TEXT NOT NULL,
		target_quality TEXT NOT NULL,
		status TEXT NOT NULL,
		progress REAL NOT NULL DEFAULT 0.0,
		error_message TEXT,
		downloaded_files_json TEXT,
		created_at DATETIME NOT NULL,
		completed_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS playlists (
		id TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		source_id TEXT NOT NULL,
		url TEXT,
		name TEXT NOT NULL,
		owner TEXT,
		image_url TEXT,
		monitored INTEGER NOT NULL DEFAULT 1,
		tracks_json TEXT,
		excluded_json TEXT,
		m3u_path TEXT,
		last_synced_at DATETIME,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);
	`

	_, err := r.db.Exec(schema)
	return err
}

func (r *SQLiteRepository) SaveArtist(ctx context.Context, artist *domain.Artist) error {
	now := time.Now()
	if artist.CreatedAt.IsZero() {
		artist.CreatedAt = now
	}
	artist.UpdatedAt = now

	monitoredInt := 0
	if artist.Monitored {
		monitoredInt = 1
	}

	query := `
	INSERT INTO artists (id, name, overview, image_url, source, source_id, monitored, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		overview = excluded.overview,
		image_url = excluded.image_url,
		monitored = excluded.monitored,
		updated_at = excluded.updated_at;
	`

	_, err := r.db.ExecContext(ctx, query,
		artist.ID,
		artist.Name,
		artist.Overview,
		artist.ImageURL,
		artist.Source,
		artist.SourceID,
		monitoredInt,
		artist.CreatedAt,
		artist.UpdatedAt,
	)
	return err
}

func (r *SQLiteRepository) GetArtist(ctx context.Context, id string) (*domain.Artist, error) {
	query := `SELECT id, name, overview, image_url, source, source_id, monitored, created_at, updated_at FROM artists WHERE id = ?`
	row := r.db.QueryRowContext(ctx, query, id)

	var a domain.Artist
	var monitoredInt int

	err := row.Scan(&a.ID, &a.Name, &a.Overview, &a.ImageURL, &a.Source, &a.SourceID, &monitoredInt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	a.Monitored = monitoredInt == 1
	return &a, nil
}

func (r *SQLiteRepository) ListArtists(ctx context.Context) ([]domain.Artist, error) {
	query := `SELECT id, name, overview, image_url, source, source_id, monitored, created_at, updated_at FROM artists ORDER BY name ASC`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artists []domain.Artist
	for rows.Next() {
		var a domain.Artist
		var monitoredInt int
		if err := rows.Scan(&a.ID, &a.Name, &a.Overview, &a.ImageURL, &a.Source, &a.SourceID, &monitoredInt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.Monitored = monitoredInt == 1
		artists = append(artists, a)
	}
	return artists, nil
}

func (r *SQLiteRepository) DeleteArtist(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM artists WHERE id = ?", id)
	return err
}

func (r *SQLiteRepository) SaveAlbum(ctx context.Context, album *domain.Album) error {
	now := time.Now()
	if album.CreatedAt.IsZero() {
		album.CreatedAt = now
	}
	album.UpdatedAt = now

	tracksBytes, _ := json.Marshal(album.Tracks)
	monitoredInt := 0
	if album.Monitored {
		monitoredInt = 1
	}

	query := `
	INSERT INTO albums (id, artist_id, artist_name, title, release_type, release_date, year, cover_url, total_tracks, upc, source, source_id, monitored, status, tracks_json, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		artist_name = excluded.artist_name,
		title = excluded.title,
		release_type = excluded.release_type,
		release_date = excluded.release_date,
		year = excluded.year,
		cover_url = excluded.cover_url,
		total_tracks = excluded.total_tracks,
		upc = excluded.upc,
		monitored = excluded.monitored,
		status = excluded.status,
		tracks_json = excluded.tracks_json,
		updated_at = excluded.updated_at;
	`

	_, err := r.db.ExecContext(ctx, query,
		album.ID,
		album.ArtistID,
		album.ArtistName,
		album.Title,
		string(album.ReleaseType),
		album.ReleaseDate,
		album.Year,
		album.CoverURL,
		album.TotalTracks,
		album.UPC,
		album.Source,
		album.SourceID,
		monitoredInt,
		string(album.Status),
		string(tracksBytes),
		album.CreatedAt,
		album.UpdatedAt,
	)
	return err
}

func (r *SQLiteRepository) GetAlbum(ctx context.Context, id string) (*domain.Album, error) {
	query := `SELECT id, artist_id, artist_name, title, release_type, release_date, year, cover_url, total_tracks, upc, source, source_id, monitored, status, tracks_json, created_at, updated_at FROM albums WHERE id = ?`
	row := r.db.QueryRowContext(ctx, query, id)

	var a domain.Album
	var monitoredInt int
	var relType, status string
	var tracksJSON sql.NullString

	err := row.Scan(&a.ID, &a.ArtistID, &a.ArtistName, &a.Title, &relType, &a.ReleaseDate, &a.Year, &a.CoverURL, &a.TotalTracks, &a.UPC, &a.Source, &a.SourceID, &monitoredInt, &status, &tracksJSON, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	a.ReleaseType = domain.ReleaseType(relType)
	a.Status = domain.DownloadStatus(status)
	a.Monitored = monitoredInt == 1

	if tracksJSON.Valid && tracksJSON.String != "" {
		_ = json.Unmarshal([]byte(tracksJSON.String), &a.Tracks)
	}

	return &a, nil
}

func (r *SQLiteRepository) ListAlbumsByArtist(ctx context.Context, artistID string) ([]domain.Album, error) {
	query := `SELECT id, artist_id, artist_name, title, release_type, release_date, year, cover_url, total_tracks, upc, source, source_id, monitored, status, tracks_json, created_at, updated_at FROM albums WHERE artist_id = ? ORDER BY year DESC, release_date DESC`
	rows, err := r.db.QueryContext(ctx, query, artistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanAlbums(rows)
}

func (r *SQLiteRepository) ListMonitoredAlbums(ctx context.Context) ([]domain.Album, error) {
	query := `SELECT id, artist_id, artist_name, title, release_type, release_date, year, cover_url, total_tracks, upc, source, source_id, monitored, status, tracks_json, created_at, updated_at FROM albums WHERE monitored = 1 ORDER BY year DESC`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanAlbums(rows)
}

func scanAlbums(rows *sql.Rows) ([]domain.Album, error) {
	var albums []domain.Album
	for rows.Next() {
		var a domain.Album
		var monitoredInt int
		var relType, status string
		var tracksJSON sql.NullString

		if err := rows.Scan(&a.ID, &a.ArtistID, &a.ArtistName, &a.Title, &relType, &a.ReleaseDate, &a.Year, &a.CoverURL, &a.TotalTracks, &a.UPC, &a.Source, &a.SourceID, &monitoredInt, &status, &tracksJSON, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.ReleaseType = domain.ReleaseType(relType)
		a.Status = domain.DownloadStatus(status)
		a.Monitored = monitoredInt == 1
		if tracksJSON.Valid && tracksJSON.String != "" {
			_ = json.Unmarshal([]byte(tracksJSON.String), &a.Tracks)
		}
		albums = append(albums, a)
	}
	return albums, nil
}

func (r *SQLiteRepository) DeleteAlbum(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM albums WHERE id = ?", id)
	return err
}

func (r *SQLiteRepository) SavePlaylist(ctx context.Context, pl *domain.Playlist) error {
	now := time.Now()
	if pl.CreatedAt.IsZero() {
		pl.CreatedAt = now
	}
	pl.UpdatedAt = now
	tracksJSON, _ := json.Marshal(pl.Tracks)
	excludedJSON, _ := json.Marshal(pl.ExcludedAlbumIDs)
	monitoredInt := 0
	if pl.Monitored {
		monitoredInt = 1
	}
	query := `
	INSERT INTO playlists (id, source, source_id, url, name, owner, image_url, monitored, tracks_json, excluded_json, m3u_path, last_synced_at, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		url = excluded.url,
		name = excluded.name,
		owner = excluded.owner,
		image_url = excluded.image_url,
		monitored = excluded.monitored,
		tracks_json = excluded.tracks_json,
		excluded_json = excluded.excluded_json,
		m3u_path = excluded.m3u_path,
		last_synced_at = excluded.last_synced_at,
		updated_at = excluded.updated_at;
	`
	_, err := r.db.ExecContext(ctx, query,
		pl.ID, pl.Source, pl.SourceID, pl.URL, pl.Name, pl.Owner, pl.ImageURL, monitoredInt,
		string(tracksJSON), string(excludedJSON), pl.M3UPath, pl.LastSyncedAt, pl.CreatedAt, pl.UpdatedAt,
	)
	return err
}

const playlistColumns = `id, source, source_id, url, name, owner, image_url, monitored, tracks_json, excluded_json, m3u_path, last_synced_at, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanPlaylist(row rowScanner) (*domain.Playlist, error) {
	var pl domain.Playlist
	var monitoredInt int
	var url, owner, image, tracksJSON, excludedJSON, m3u sql.NullString
	var synced sql.NullTime
	if err := row.Scan(&pl.ID, &pl.Source, &pl.SourceID, &url, &pl.Name, &owner, &image, &monitoredInt,
		&tracksJSON, &excludedJSON, &m3u, &synced, &pl.CreatedAt, &pl.UpdatedAt); err != nil {
		return nil, err
	}
	pl.URL, pl.Owner, pl.ImageURL, pl.M3UPath = url.String, owner.String, image.String, m3u.String
	pl.Monitored = monitoredInt == 1
	if synced.Valid {
		pl.LastSyncedAt = synced.Time
	}
	if tracksJSON.Valid && tracksJSON.String != "" {
		_ = json.Unmarshal([]byte(tracksJSON.String), &pl.Tracks)
	}
	if excludedJSON.Valid && excludedJSON.String != "" {
		_ = json.Unmarshal([]byte(excludedJSON.String), &pl.ExcludedAlbumIDs)
	}
	return &pl, nil
}

func (r *SQLiteRepository) GetPlaylist(ctx context.Context, id string) (*domain.Playlist, error) {
	row := r.db.QueryRowContext(ctx, "SELECT "+playlistColumns+" FROM playlists WHERE id = ?", id)
	pl, err := scanPlaylist(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return pl, err
}

func (r *SQLiteRepository) ListPlaylists(ctx context.Context) ([]domain.Playlist, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+playlistColumns+" FROM playlists ORDER BY name COLLATE NOCASE ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Playlist
	for rows.Next() {
		pl, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *pl)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) DeletePlaylist(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM playlists WHERE id = ?", id)
	return err
}

func (r *SQLiteRepository) SaveDownloadJob(ctx context.Context, job *domain.DownloadJob) error {
	filesJSON, _ := json.Marshal(job.DownloadedFiles)
	query := `
	INSERT INTO download_jobs (id, album_id, downloader, download_dir, target_quality, status, progress, error_message, downloaded_files_json, created_at, completed_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		status = excluded.status,
		progress = excluded.progress,
		error_message = excluded.error_message,
		downloaded_files_json = excluded.downloaded_files_json,
		completed_at = excluded.completed_at;
	`
	_, err := r.db.ExecContext(ctx, query,
		job.ID,
		job.AlbumID,
		job.Downloader,
		job.DownloadDir,
		string(job.TargetQuality),
		string(job.Status),
		job.Progress,
		job.ErrorMessage,
		string(filesJSON),
		job.CreatedAt,
		job.CompletedAt,
	)
	return err
}

func (r *SQLiteRepository) GetDownloadJob(ctx context.Context, id string) (*domain.DownloadJob, error) {
	query := `SELECT id, album_id, downloader, download_dir, target_quality, status, progress, error_message, downloaded_files_json, created_at, completed_at FROM download_jobs WHERE id = ?`
	row := r.db.QueryRowContext(ctx, query, id)

	var j domain.DownloadJob
	var quality, status string
	var filesJSON sql.NullString
	var completedAt sql.NullTime

	err := row.Scan(&j.ID, &j.AlbumID, &j.Downloader, &j.DownloadDir, &quality, &status, &j.Progress, &j.ErrorMessage, &filesJSON, &j.CreatedAt, &completedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	j.TargetQuality = domain.AudioQuality(quality)
	j.Status = domain.DownloadStatus(status)
	if completedAt.Valid {
		j.CompletedAt = &completedAt.Time
	}
	if filesJSON.Valid && filesJSON.String != "" {
		_ = json.Unmarshal([]byte(filesJSON.String), &j.DownloadedFiles)
	}

	return &j, nil
}

func (r *SQLiteRepository) ListActiveDownloadJobs(ctx context.Context) ([]domain.DownloadJob, error) {
	query := `SELECT id, album_id, downloader, download_dir, target_quality, status, progress, error_message, downloaded_files_json, created_at, completed_at FROM download_jobs WHERE status IN ('pending', 'downloading', 'verifying', 'importing')`
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.DownloadJob
	for rows.Next() {
		var j domain.DownloadJob
		var quality, status string
		var filesJSON sql.NullString
		var completedAt sql.NullTime

		if err := rows.Scan(&j.ID, &j.AlbumID, &j.Downloader, &j.DownloadDir, &quality, &status, &j.Progress, &j.ErrorMessage, &filesJSON, &j.CreatedAt, &completedAt); err != nil {
			return nil, err
		}
		j.TargetQuality = domain.AudioQuality(quality)
		j.Status = domain.DownloadStatus(status)
		if completedAt.Valid {
			j.CompletedAt = &completedAt.Time
		}
		if filesJSON.Valid && filesJSON.String != "" {
			_ = json.Unmarshal([]byte(filesJSON.String), &j.DownloadedFiles)
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (r *SQLiteRepository) ListDownloadJobs(ctx context.Context, limit int) ([]domain.DownloadJob, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, album_id, downloader, download_dir, target_quality, status, progress, error_message, downloaded_files_json, created_at, completed_at FROM download_jobs ORDER BY created_at DESC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.DownloadJob
	for rows.Next() {
		var j domain.DownloadJob
		var quality, status string
		var filesJSON sql.NullString
		var completedAt sql.NullTime

		if err := rows.Scan(&j.ID, &j.AlbumID, &j.Downloader, &j.DownloadDir, &quality, &status, &j.Progress, &j.ErrorMessage, &filesJSON, &j.CreatedAt, &completedAt); err != nil {
			return nil, err
		}
		j.TargetQuality = domain.AudioQuality(quality)
		j.Status = domain.DownloadStatus(status)
		if completedAt.Valid {
			j.CompletedAt = &completedAt.Time
		}
		if filesJSON.Valid && filesJSON.String != "" {
			_ = json.Unmarshal([]byte(filesJSON.String), &j.DownloadedFiles)
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (r *SQLiteRepository) ClearDownloadHistory(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM download_jobs WHERE status IN ('completed', 'failed')")
	return err
}

func (r *SQLiteRepository) Close() error {
	return r.db.Close()
}
