package downloader

import (
	"context"
	"fmt"
	"os"

	"github.com/findr-app/findr/internal/domain"
)

// FallbackDownloader coordinates downloading across a primary downloader and a fallback downloader.
type FallbackDownloader struct {
	primary  Downloader
	fallback Downloader
}

// NewFallbackDownloader creates a new composite fallback downloader.
func NewFallbackDownloader(primary, fallback Downloader) *FallbackDownloader {
	return &FallbackDownloader{
		primary:  primary,
		fallback: fallback,
	}
}

// Name returns the composite name of the downloaders.
func (d *FallbackDownloader) Name() string {
	if d.primary != nil && d.fallback != nil {
		return fmt.Sprintf("%s (fallback: %s)", d.primary.Name(), d.fallback.Name())
	}
	if d.primary != nil {
		return d.primary.Name()
	}
	if d.fallback != nil {
		return d.fallback.Name()
	}
	return "none"
}

// Primary returns the primary downloader instance.
func (d *FallbackDownloader) Primary() Downloader {
	return d.primary
}

// Fallback returns the fallback downloader instance.
func (d *FallbackDownloader) Fallback() Downloader {
	return d.fallback
}

// Search queries the primary downloader first, cascading to fallback if empty or on error.
func (d *FallbackDownloader) Search(ctx context.Context, album *domain.Album) ([]SearchResult, error) {
	if d.primary != nil {
		results, err := d.primary.Search(ctx, album)
		if err == nil && len(results) > 0 {
			return results, nil
		}
	}
	if d.fallback != nil {
		return d.fallback.Search(ctx, album)
	}
	return nil, fmt.Errorf("no downloader available")
}

// Download attempts downloading the album via primary; if primary fails or is unavailable,
// it cleans up any incomplete staging files and delegates to the fallback downloader.
func (d *FallbackDownloader) Download(ctx context.Context, album *domain.Album, targetDir string, progress ProgressFunc) (*domain.DownloadJob, error) {
	var primaryErr error
	if d.primary != nil {
		job, err := d.primary.Download(ctx, album, targetDir, progress)
		if err == nil && job != nil && job.Status != domain.StatusFailed {
			return job, nil
		}
		primaryErr = err
		if d.fallback == nil {
			return job, err
		}

		fmt.Printf("[findr Downloader] Primary (%s) failed for '%s - %s': %v. Cascading to fallback (%s)...\n",
			d.primary.Name(), album.ArtistName, album.Title, err, d.fallback.Name())

		// Reset target directory before attempting fallback download
		_ = os.RemoveAll(targetDir)
		_ = os.MkdirAll(targetDir, 0755)
	}

	if d.fallback != nil {
		if progress != nil {
			progress(5.0, fmt.Sprintf("Primary downloader unavailable (%v). Falling back to %s for '%s - %s'...",
				primaryErr, d.fallback.Name(), album.ArtistName, album.Title))
		}
		return d.fallback.Download(ctx, album, targetDir, progress)
	}

	return nil, fmt.Errorf("no downloaders available")
}

// DownloadTrack attempts downloading a single track via primary (if supported), falling back to fallback downloader.
func (d *FallbackDownloader) DownloadTrack(ctx context.Context, artist string, track domain.Track, destPath string, progress ProgressFunc) (string, error) {
	if pt, ok := d.primary.(TrackDownloader); ok && pt != nil {
		actualPath, err := pt.DownloadTrack(ctx, artist, track, destPath, progress)
		if err == nil {
			return actualPath, nil
		}
		fmt.Printf("[findr Downloader] Primary (%s) track download failed for '%s - %s': %v. Cascading to fallback...\n",
			d.primary.Name(), artist, track.Title, err)
	}

	if fb, ok := d.fallback.(TrackDownloader); ok && fb != nil {
		return fb.DownloadTrack(ctx, artist, track, destPath, progress)
	}

	return "", fmt.Errorf("no track downloader available for '%s - %s'", artist, track.Title)
}
