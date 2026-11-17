package verifier

import (
	"context"
	"testing"

	"github.com/findr-app/findr/internal/domain"
)

func TestStringSimilarity(t *testing.T) {
	tests := []struct {
		s1       string
		s2       string
		minScore float64
	}{
		{"Get Lucky", "Get Lucky", 1.0},
		{"Get Lucky (feat. Pharrell Williams)", "Get Lucky", 0.70},
		{"01 - Around the World", "Around the World", 0.80},
		{"Harder, Better, Faster, Stronger", "Harder Better Faster Stronger", 0.95},
		{"Completely Different Track", "One More Time", 0.0},
		{"?", "_", 1.0},
		{"+", "+", 1.0},
		{"$$$", "$$$", 1.0},
	}

	for _, tt := range tests {
		score := StringSimilarity(tt.s1, tt.s2)
		if score < tt.minScore {
			t.Errorf("StringSimilarity(%q, %q) = %.2f; expected >= %.2f", tt.s1, tt.s2, score, tt.minScore)
		}
	}
}

func TestParseFilename(t *testing.T) {
	tests := []struct {
		filename      string
		expectedDisc  int
		expectedTrack int
		expectedTitle string
	}{
		{"01 - Around the World", 1, 1, "Around the World"},
		{"1-03 - Aerodynamic", 1, 3, "Aerodynamic"},
		{"2-05 - Voyager", 2, 5, "Voyager"},
		{"04 Digital Love", 1, 4, "Digital Love"},
		{"CD 1 - 02 - Superheroes", 1, 2, "Superheroes"},
		{"Disc 2 - 05 - High Life", 2, 5, "High Life"},
		{"07 - _", 1, 7, "_"},
	}

	for _, tt := range tests {
		disc, track, title := parseFilename(tt.filename)
		if disc != tt.expectedDisc || track != tt.expectedTrack || title != tt.expectedTitle {
			t.Errorf("parseFilename(%q) = (%d, %d, %q); expected (%d, %d, %q)",
				tt.filename, disc, track, title, tt.expectedDisc, tt.expectedTrack, tt.expectedTitle)
		}
	}
}

func TestVerifyAlbumSuccess(t *testing.T) {
	v := NewVerifier(Config{
		DurationToleranceSec: 4,
		MinConfidenceScore:   0.85,
		AllowPartialMatch:    false,
	})

	album := &domain.Album{
		ID:          "123",
		ArtistName:  "Daft Punk",
		Title:       "Discovery",
		TotalTracks: 3,
		Tracks: []domain.Track{
			{ID: "t1", DiscNumber: 1, TrackNumber: 1, Title: "One More Time", DurationSec: 320},
			{ID: "t2", DiscNumber: 1, TrackNumber: 2, Title: "Aerodynamic", DurationSec: 207},
			{ID: "t3", DiscNumber: 1, TrackNumber: 3, Title: "Digital Love", DurationSec: 298},
		},
	}

	candidates := []domain.CandidateFile{
		{Path: "/tmp/01 - One More Time.flac", Filename: "01 - One More Time.flac", TrackNumber: 1, DiscNumber: 1, Title: "One More Time", DurationSec: 321},
		{Path: "/tmp/02 - Aerodynamic.flac", Filename: "02 - Aerodynamic.flac", TrackNumber: 2, DiscNumber: 1, Title: "Aerodynamic", DurationSec: 206},
		{Path: "/tmp/03 - Digital Love.flac", Filename: "03 - Digital Love.flac", TrackNumber: 3, DiscNumber: 1, Title: "Digital Love", DurationSec: 300},
	}

	result, err := v.VerifyAlbum(context.Background(), album, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Passed {
		t.Errorf("expected verification to pass, failed with reason: %s", result.Reason)
	}
	if len(result.MatchedTracks) != 3 {
		t.Errorf("expected 3 matched tracks, got %d", len(result.MatchedTracks))
	}
	if result.ConfidenceScore < 0.90 {
		t.Errorf("expected confidence >= 0.90, got %.2f", result.ConfidenceScore)
	}
}

func TestVerifyAlbumSymbolTrack(t *testing.T) {
	v := NewVerifier(Config{
		DurationToleranceSec: 4,
		MinConfidenceScore:   0.85,
		AllowPartialMatch:    false,
	})

	album := &domain.Album{
		ID:          "doom-best-of",
		ArtistName:  "MF DOOM",
		Title:       "Best of Mf",
		TotalTracks: 2,
		Tracks: []domain.Track{
			{ID: "t1", DiscNumber: 1, TrackNumber: 1, Title: "Foolish", DurationSec: 120},
			{ID: "t2", DiscNumber: 1, TrackNumber: 2, Title: "?", DurationSec: 180},
		},
	}

	candidates := []domain.CandidateFile{
		{Path: "/tmp/01 - Foolish.flac", Filename: "01 - Foolish.flac", TrackNumber: 1, DiscNumber: 1, Title: "Foolish", DurationSec: 120},
		{Path: "/tmp/02 - _.flac", Filename: "02 - _.flac", TrackNumber: 2, DiscNumber: 1, Title: "_", DurationSec: 180},
	}

	result, err := v.VerifyAlbum(context.Background(), album, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Passed {
		t.Errorf("expected verification to pass for symbol track '?', failed with reason: %s", result.Reason)
	}
	if len(result.MatchedTracks) != 2 {
		t.Errorf("expected 2 matched tracks, got %d", len(result.MatchedTracks))
	}
}

func TestVerifyAlbumMissingTrackFailure(t *testing.T) {
	v := NewVerifier(Config{
		DurationToleranceSec: 4,
		MinConfidenceScore:   0.85,
		AllowPartialMatch:    false,
	})

	album := &domain.Album{
		ID:          "123",
		ArtistName:  "Daft Punk",
		Title:       "Discovery",
		TotalTracks: 3,
		Tracks: []domain.Track{
			{ID: "t1", TrackNumber: 1, Title: "One More Time", DurationSec: 320},
			{ID: "t2", TrackNumber: 2, Title: "Aerodynamic", DurationSec: 207},
			{ID: "t3", TrackNumber: 3, Title: "Digital Love", DurationSec: 298},
		},
	}

	// Missing track 3
	candidates := []domain.CandidateFile{
		{Path: "/tmp/01 - One More Time.flac", Filename: "01 - One More Time.flac", TrackNumber: 1, Title: "One More Time", DurationSec: 320},
		{Path: "/tmp/02 - Aerodynamic.flac", Filename: "02 - Aerodynamic.flac", TrackNumber: 2, Title: "Aerodynamic", DurationSec: 207},
	}

	result, err := v.VerifyAlbum(context.Background(), album, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Passed {
		t.Errorf("expected verification to fail due to missing track, but passed")
	}
	if len(result.UnmatchedTracks) != 1 {
		t.Errorf("expected 1 unmatched track, got %d", len(result.UnmatchedTracks))
	}
}
