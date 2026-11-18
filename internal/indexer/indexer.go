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

// AlbumSearcher is implemented by indexers that can search releases directly by title.
type AlbumSearcher interface {
	SearchAlbum(ctx context.Context, query string) ([]domain.Album, error)
}

// TrackHit is a track search result together with the release it belongs to.
type TrackHit struct {
	TrackID     string `json:"track_id"`
	Title       string `json:"title"`
	ArtistName  string `json:"artist_name"`
	DurationSec int    `json:"duration_sec"`
	ISRC        string `json:"isrc,omitempty"`
	AlbumID     string `json:"album_id"`
	AlbumTitle  string `json:"album_title"`
	CoverURL    string `json:"cover_url,omitempty"`
}

// TrackSearcher is implemented by indexers that can resolve individual tracks
// (used to map external playlist entries onto indexer releases).
type TrackSearcher interface {
	SearchTrack(ctx context.Context, artist, title string) ([]TrackHit, error)
}

// ISRCResolver is implemented by indexers that can look a track up by ISRC.
type ISRCResolver interface {
	LookupISRC(ctx context.Context, isrc string) (*TrackHit, error)
}

// TrackGetter is implemented by indexers that can fetch one track by its ID
// (used to resolve track links to the release they belong to).
type TrackGetter interface {
	GetTrack(ctx context.Context, id string) (*TrackHit, error)
}
