package downloader

import (
	"context"

	"github.com/findr-app/findr/internal/domain"
)

// SearchResult represents a candidate match discovered by a downloader.
type SearchResult struct {
	ID          string              `json:"id"`
	Title       string              `json:"title"`
	Artist      string              `json:"artist"`
	Format      domain.AudioQuality `json:"format"`
	Bitrate     int                 `json:"bitrate,omitempty"`
	SampleRate  int                 `json:"sample_rate,omitempty"`
	BitDepth    int                 `json:"bit_depth,omitempty"`
	TotalTracks int                 `json:"total_tracks"`
	Size        int64               `json:"size"`
	Source      string              `json:"source"` // "qobuz", "slskd"
	SourceData  map[string]string   `json:"source_data,omitempty"`
}

// ProgressFunc reports download percentage and message.
type ProgressFunc func(progress float64, message string)

// Downloader is the interface implemented by music fetchers (Qobuz, slskd/Soulseek).
type Downloader interface {
	// Name returns downloader identifier.
	Name() string

	// Search searches for a matching album or release on the source network.
	Search(ctx context.Context, album *domain.Album) ([]SearchResult, error)

	// Download fetches the release into targetDir.
	Download(ctx context.Context, album *domain.Album, targetDir string, progress ProgressFunc) (*domain.DownloadJob, error)
}

// TrackDownloader is implemented by downloaders capable of downloading individual tracks.
type TrackDownloader interface {
	DownloadTrack(ctx context.Context, artist string, track domain.Track, destPath string, progress ProgressFunc) (string, error)
}

