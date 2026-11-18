package domain

import (
	"time"
)

// ReleaseType classifies the release format.
type ReleaseType string

const (
	ReleaseTypeAlbum       ReleaseType = "album"
	ReleaseTypeEP          ReleaseType = "ep"
	ReleaseTypeSingle      ReleaseType = "single"
	ReleaseTypeCompilation ReleaseType = "compilation"
	ReleaseTypeLive        ReleaseType = "live"
	ReleaseTypeOther       ReleaseType = "other"
)

// AudioQuality defines target audio quality level.
type AudioQuality string

const (
	QualityMP3320   AudioQuality = "mp3_320"
	QualityFLAC16   AudioQuality = "flac_16_44"  // Lossless CD quality (16-bit / 44.1kHz)
	QualityFLAC2496 AudioQuality = "flac_24_96"  // Hi-Res Lossless (24-bit / up to 96kHz)
	QualityFLAC2419 AudioQuality = "flac_24_192" // Hi-Res Lossless (24-bit / up to 192kHz)
)

// DownloadStatus represents the lifecycle of a download and import job.
type DownloadStatus string

const (
	StatusPending     DownloadStatus = "pending"
	StatusDownloading DownloadStatus = "downloading"
	StatusDownloaded  DownloadStatus = "downloaded"
	StatusVerifying   DownloadStatus = "verifying"
	StatusImporting   DownloadStatus = "importing"
	StatusCompleted   DownloadStatus = "completed"
	StatusFailed      DownloadStatus = "failed"
)

// Artist represents a musician, band, or creator.
type Artist struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Overview  string    `json:"overview,omitempty"`
	ImageURL  string    `json:"image_url,omitempty"`
	Source    string    `json:"source"` // "deezer", "qobuz", "spotify"
	SourceID  string    `json:"source_id"`
	Monitored bool      `json:"monitored"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Album represents a release (album, EP, single).
type Album struct {
	ID          string         `json:"id"`
	ArtistID    string         `json:"artist_id"`
	ArtistName  string         `json:"artist_name"`
	Title       string         `json:"title"`
	ReleaseType ReleaseType    `json:"release_type"`
	ReleaseDate string         `json:"release_date"` // YYYY-MM-DD
	Year        int            `json:"year"`
	CoverURL    string         `json:"cover_url,omitempty"`
	TotalTracks int            `json:"total_tracks"`
	UPC         string         `json:"upc,omitempty"`
	Source      string         `json:"source"`
	SourceID    string         `json:"source_id"`
	Monitored   bool           `json:"monitored"`
	Status      DownloadStatus `json:"status"`
	Tracks      []Track        `json:"tracks,omitempty"`
	// WantedTrackIDs limits downloads to these tracks (Track.ID). Empty means
	// the whole release.
	WantedTrackIDs []string  `json:"wanted_track_ids,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// IsPartial reports whether only some of the release's tracks are wanted.
func (a *Album) IsPartial() bool { return len(a.WantedTrackIDs) > 0 }

// WantedTracks returns the tracks to download: the picked ones for a partial
// release, every track otherwise. Picked IDs that are not in the tracklist are
// ignored; if none match, the whole tracklist is returned.
func (a *Album) WantedTracks() []Track {
	if !a.IsPartial() {
		return a.Tracks
	}
	want := make(map[string]bool, len(a.WantedTrackIDs))
	for _, id := range a.WantedTrackIDs {
		want[id] = true
	}
	out := make([]Track, 0, len(a.WantedTrackIDs))
	for _, t := range a.Tracks {
		if want[t.ID] || (t.SourceID != "" && want[t.SourceID]) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return a.Tracks
	}
	return out
}

// Track represents an individual audio track within an album.
type Track struct {
	ID          string `json:"id"`
	AlbumID     string `json:"album_id"`
	ArtistName  string `json:"artist_name"`
	Title       string `json:"title"`
	DiscNumber  int    `json:"disc_number"`
	TrackNumber int    `json:"track_number"`
	DurationSec int    `json:"duration_sec"` // Duration in seconds
	ISRC        string `json:"isrc,omitempty"`
	Explicit    bool   `json:"explicit"`
	SourceID    string `json:"source_id,omitempty"`
	FilePath    string `json:"file_path,omitempty"`
}

// CandidateFile represents a local downloaded audio file to be verified.
type CandidateFile struct {
	Path        string       `json:"path"`
	Filename    string       `json:"filename"`
	Extension   string       `json:"extension"`
	Size        int64        `json:"size"`
	DurationSec int          `json:"duration_sec"`
	Artist      string       `json:"artist,omitempty"`
	Album       string       `json:"album,omitempty"`
	Title       string       `json:"title,omitempty"`
	TrackNumber int          `json:"track_number,omitempty"`
	DiscNumber  int          `json:"disc_number,omitempty"`
	Bitrate     int          `json:"bitrate,omitempty"`
	SampleRate  int          `json:"sample_rate,omitempty"`
	BitDepth    int          `json:"bit_depth,omitempty"`
	Format      AudioQuality `json:"format,omitempty"`
}

// TrackMatch pairs an expected track with a discovered candidate file.
type TrackMatch struct {
	Track        Track         `json:"track"`
	File         CandidateFile `json:"file"`
	Score        float64       `json:"score"` // 0.0 - 1.0 confidence
	DurationDiff int           `json:"duration_diff_sec"`
}

// MatchResult is the verification output for an album download.
type MatchResult struct {
	AlbumID             string          `json:"album_id"`
	TotalExpectedTracks int             `json:"total_expected_tracks"`
	TotalCandidateFiles int             `json:"total_candidate_files"`
	MatchedTracks       []TrackMatch    `json:"matched_tracks"`
	UnmatchedTracks     []Track         `json:"unmatched_tracks"`
	UnmatchedFiles      []CandidateFile `json:"unmatched_files"`
	ConfidenceScore     float64         `json:"confidence_score"` // 0.0 to 1.0
	Passed              bool            `json:"passed"`
	Reason              string          `json:"reason,omitempty"`
}

// DownloadJob tracks an ongoing or completed download.
type DownloadJob struct {
	ID              string         `json:"id"`
	AlbumID         string         `json:"album_id"`
	Downloader      string         `json:"downloader"` // "qobuz", "slskd"
	DownloadDir     string         `json:"download_dir"`
	TargetQuality   AudioQuality   `json:"target_quality"`
	Status          DownloadStatus `json:"status"`
	Progress        float64        `json:"progress"` // 0.0 to 100.0
	ErrorMessage    string         `json:"error_message,omitempty"`
	DownloadedFiles []string       `json:"downloaded_files,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	CompletedAt     *time.Time     `json:"completed_at,omitempty"`
}

// Playlist is an external playlist (e.g. Spotify) whose tracks are resolved to
// indexer albums so findr can track and download the releases they come from.
type Playlist struct {
	ID        string          `json:"id"`
	Source    string          `json:"source"` // "spotify"
	SourceID  string          `json:"source_id"`
	URL       string          `json:"url"`
	Name      string          `json:"name"`
	Owner     string          `json:"owner,omitempty"`
	ImageURL  string          `json:"image_url,omitempty"`
	Monitored bool            `json:"monitored"`
	Tracks    []PlaylistTrack `json:"tracks"`
	// ExcludedAlbumIDs are albums the user chose not to track; syncs skip them.
	ExcludedAlbumIDs []string  `json:"excluded_album_ids,omitempty"`
	M3UPath          string    `json:"m3u_path,omitempty"`
	LastSyncedAt     time.Time `json:"last_synced_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// PlaylistTrack is one playlist entry and the indexer release it resolved to.
type PlaylistTrack struct {
	Position    int      `json:"position"`
	Title       string   `json:"title"`
	Artists     []string `json:"artists"`
	AlbumTitle  string   `json:"album_title,omitempty"`
	DurationSec int      `json:"duration_sec"`
	ISRC        string   `json:"isrc,omitempty"`
	SourceURI   string   `json:"source_uri,omitempty"`

	// Resolution against the active indexer. AlbumID is empty when unresolved.
	AlbumID         string  `json:"album_id,omitempty"`
	MatchedTitle    string  `json:"matched_title,omitempty"`
	MatchedArtist   string  `json:"matched_artist,omitempty"`
	MatchedAlbum    string  `json:"matched_album,omitempty"`
	MatchedCoverURL string  `json:"matched_cover_url,omitempty"`
	MatchScore      float64 `json:"match_score,omitempty"`
}
