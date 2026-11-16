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
	QualityFLAC16   AudioQuality = "flac_16_44"   // Lossless CD quality (16-bit / 44.1kHz)
	QualityFLAC2496 AudioQuality = "flac_24_96"   // Hi-Res Lossless (24-bit / up to 96kHz)
	QualityFLAC2419 AudioQuality = "flac_24_192"  // Hi-Res Lossless (24-bit / up to 192kHz)
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
	ID          string       `json:"id"`
	ArtistID    string       `json:"artist_id"`
	ArtistName  string       `json:"artist_name"`
	Title       string       `json:"title"`
	ReleaseType ReleaseType  `json:"release_type"`
	ReleaseDate string       `json:"release_date"` // YYYY-MM-DD
	Year        int          `json:"year"`
	CoverURL    string       `json:"cover_url,omitempty"`
	TotalTracks int          `json:"total_tracks"`
	UPC         string       `json:"upc,omitempty"`
	Source      string       `json:"source"`
	SourceID    string       `json:"source_id"`
	Monitored   bool         `json:"monitored"`
	Status      DownloadStatus `json:"status"`
	Tracks      []Track      `json:"tracks,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Track represents an individual audio track within an album.
type Track struct {
	ID          string  `json:"id"`
	AlbumID     string  `json:"album_id"`
	ArtistName  string  `json:"artist_name"`
	Title       string  `json:"title"`
	DiscNumber  int     `json:"disc_number"`
	TrackNumber int     `json:"track_number"`
	DurationSec int     `json:"duration_sec"` // Duration in seconds
	ISRC        string  `json:"isrc,omitempty"`
	Explicit    bool    `json:"explicit"`
	SourceID    string  `json:"source_id,omitempty"`
	FilePath    string  `json:"file_path,omitempty"`
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
	Track       Track         `json:"track"`
	File        CandidateFile `json:"file"`
	Score       float64       `json:"score"` // 0.0 - 1.0 confidence
	DurationDiff int          `json:"duration_diff_sec"`
}

// MatchResult is the verification output for an album download.
type MatchResult struct {
	AlbumID              string        `json:"album_id"`
	TotalExpectedTracks  int           `json:"total_expected_tracks"`
	TotalCandidateFiles  int           `json:"total_candidate_files"`
	MatchedTracks        []TrackMatch  `json:"matched_tracks"`
	UnmatchedTracks      []Track       `json:"unmatched_tracks"`
	UnmatchedFiles       []CandidateFile `json:"unmatched_files"`
	ConfidenceScore      float64       `json:"confidence_score"` // 0.0 to 1.0
	Passed               bool          `json:"passed"`
	Reason               string        `json:"reason,omitempty"`
}

// DownloadJob tracks an ongoing or completed download.
type DownloadJob struct {
	ID             string         `json:"id"`
	AlbumID        string         `json:"album_id"`
	Downloader     string         `json:"downloader"` // "qobuz", "slskd"
	DownloadDir    string         `json:"download_dir"`
	TargetQuality  AudioQuality   `json:"target_quality"`
	Status         DownloadStatus `json:"status"`
	Progress       float64        `json:"progress"` // 0.0 to 100.0
	ErrorMessage   string         `json:"error_message,omitempty"`
	DownloadedFiles []string      `json:"downloaded_files,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	CompletedAt    *time.Time     `json:"completed_at,omitempty"`
}
