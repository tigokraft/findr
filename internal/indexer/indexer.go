package indexer

import (
	"context"

	"github.com/findr-app/findr/internal/domain"
)

// ReleaseFilter allows querying specific release types or dates.
type ReleaseFilter struct {
	Types []domain.ReleaseType // If empty, all types are included
	Limit int                  // Maximum releases to fetch
}

// Indexer is the primary contract for artist and album metadata providers.
type Indexer interface {
	// Name returns the provider's identifier (e.g. "deezer", "qobuz", "spotify").
	Name() string

	// SearchArtist searches for artists matching the given query string.
	SearchArtist(ctx context.Context, query string) ([]domain.Artist, error)

	// GetArtist fetches an artist's profile by provider ID.
	GetArtist(ctx context.Context, id string) (*domain.Artist, error)

	// GetArtistReleases retrieves the discography (albums, EPs, singles) for an artist.
	GetArtistReleases(ctx context.Context, artistID string, filter ReleaseFilter) ([]domain.Album, error)

	// GetAlbumDetails retrieves full metadata for an album, including its tracklist.
	GetAlbumDetails(ctx context.Context, albumID string) (*domain.Album, error)
}
