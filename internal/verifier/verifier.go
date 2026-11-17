package verifier

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/findr-app/findr/internal/domain"
)

var (
	// Regex patterns for extracting track numbers and discs from filenames
	trackWithDiscPrefixRegex = regexp.MustCompile(`(?i)^(?:cd|disc|disk)\s*(\d{1,2})[\s/\\_.-]+(\d{1,3})(?:\s*[-.]\s*|\s+|_\s*)(.+)$`) // "CD 1 - 02 Track"
	trackWithDiscRegex       = regexp.MustCompile(`^(\d{1,2})[-_.](\d{1,3})(?:\s*[-.]\s*|\s+|_\s*)(.+)$`)                              // "1-02 Track Name" or "1.02 Track"
	trackSimpleRegex         = regexp.MustCompile(`^(\d{1,3})(?:\s*[-.]\s*|\s+|_\s*)(.+)$`)                                            // "02 - Track Name" or "02 Track Name"
	// "Artist - 02 - Track", "Artist - Album - 1-02 - Track", "Artist - 02. Track"
	trackEmbeddedRegex = regexp.MustCompile(`^.+?\s+-\s+(?:(\d{1,2})[-.])?(\d{1,3})(?:\s*-\s+|\.\s+)(.+)$`)
	validAudioExts     = map[string]bool{
		".flac": true,
		".mp3":  true,
		".m4a":  true,
		".wav":  true,
		".ogg":  true,
		".opus": true,
		".aiff": true,
	}
)

// Config configures the verification rules.
type Config struct {
	DurationToleranceSec int     // e.g. 4 seconds
	MinConfidenceScore   float64 // e.g. 0.85
	AllowPartialMatch    bool    // e.g. false
}

// Verifier executes validation and matching between downloaded files and indexer metadata.
type Verifier struct {
	cfg Config
}

// NewVerifier initializes a new verification engine.
func NewVerifier(cfg Config) *Verifier {
	if cfg.DurationToleranceSec <= 0 {
		cfg.DurationToleranceSec = 4
	}
	if cfg.MinConfidenceScore <= 0 {
		cfg.MinConfidenceScore = 0.85
	}
	return &Verifier{cfg: cfg}
}

// ScanCandidateFiles traverses the download directory and identifies audio files.
func (v *Verifier) ScanCandidateFiles(dir string) ([]domain.CandidateFile, error) {
	var candidates []domain.CandidateFile

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !validAudioExts[ext] {
			return nil
		}

		filename := filepath.Base(path)
		filenameNoExt := strings.TrimSuffix(filename, ext)

		disc, trackNum, parsedTitle := parseFilename(filenameNoExt)

		candidates = append(candidates, domain.CandidateFile{
			Path:        path,
			Filename:    filename,
			Extension:   ext,
			Size:        info.Size(),
			TrackNumber: trackNum,
			DiscNumber:  disc,
			Title:       parsedTitle,
		})

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("scan candidates in %s: %w", dir, err)
	}

	return candidates, nil
}

// VerifyAlbum checks candidate files against canonical album and tracklist metadata.
func (v *Verifier) VerifyAlbum(ctx context.Context, album *domain.Album, candidates []domain.CandidateFile) (*domain.MatchResult, error) {
	result := &domain.MatchResult{
		AlbumID:             album.ID,
		TotalExpectedTracks: len(album.Tracks),
		TotalCandidateFiles: len(candidates),
		MatchedTracks:       make([]domain.TrackMatch, 0),
		UnmatchedTracks:     make([]domain.Track, 0),
		UnmatchedFiles:      make([]domain.CandidateFile, 0),
	}

	if len(album.Tracks) == 0 {
		result.Passed = false
		result.Reason = "expected album tracklist is empty"
		return result, nil
	}

	if len(candidates) == 0 {
		result.Passed = false
		result.Reason = "no candidate audio files found in download directory"
		return result, nil
	}

	usedCandidates := make(map[string]bool)
	var totalScore float64

	// Match expected tracks to candidate files using greedy scoring
	for _, expected := range album.Tracks {
		var bestMatch *domain.CandidateFile
		var bestScore float64
		var bestDurationDiff int

		for i := range candidates {
			cand := &candidates[i]
			if usedCandidates[cand.Path] {
				continue
			}

			score, durationDiff := v.scoreMatch(expected, *cand)
			if score > bestScore {
				bestScore = score
				bestMatch = cand
				bestDurationDiff = durationDiff
			}
		}

		// Require at least a baseline score (0.45) to accept match
		if bestMatch != nil && bestScore >= 0.45 {
			usedCandidates[bestMatch.Path] = true
			totalScore += bestScore
			result.MatchedTracks = append(result.MatchedTracks, domain.TrackMatch{
				Track:        expected,
				File:         *bestMatch,
				Score:        bestScore,
				DurationDiff: bestDurationDiff,
			})
		} else {
			result.UnmatchedTracks = append(result.UnmatchedTracks, expected)
		}
	}

	// Identify remaining unused files
	for _, cand := range candidates {
		if !usedCandidates[cand.Path] {
			result.UnmatchedFiles = append(result.UnmatchedFiles, cand)
		}
	}

	// Calculate overall confidence score
	confidence := totalScore / float64(len(album.Tracks))
	result.ConfidenceScore = confidence

	// Assess pass / fail criteria
	switch {
	case !v.cfg.AllowPartialMatch && len(result.UnmatchedTracks) > 0:
		result.Passed = false
		result.Reason = fmt.Sprintf("missing %d required tracks from expected album (matched %d/%d)",
			len(result.UnmatchedTracks), len(result.MatchedTracks), len(album.Tracks))
	case confidence < v.cfg.MinConfidenceScore:
		result.Passed = false
		result.Reason = fmt.Sprintf("confidence score %.2f is below minimum threshold %.2f",
			confidence, v.cfg.MinConfidenceScore)
	default:
		result.Passed = true
		result.Reason = fmt.Sprintf("successfully verified %d/%d tracks with confidence %.2f",
			len(result.MatchedTracks), len(album.Tracks), confidence)
	}

	return result, nil
}

// scoreMatch evaluates alignment between an expected track and a candidate file.
// Maximum score is 1.0:
// - Track number match: 0.40
// - Title similarity: 0.40
// - Duration match within tolerance: 0.20
func (v *Verifier) scoreMatch(expected domain.Track, cand domain.CandidateFile) (float64, int) {
	var score float64

	// 1. Track Number Alignment (0.40 weight)
	if cand.TrackNumber > 0 && cand.TrackNumber == expected.TrackNumber {
		if cand.DiscNumber == 0 || expected.DiscNumber == 0 || cand.DiscNumber == expected.DiscNumber {
			score += 0.40
		} else {
			score += 0.20
		}
	}

	// 2. Title Similarity (0.40 weight)
	titleToCompare := cand.Title
	if titleToCompare == "" {
		titleToCompare = cand.Filename
	}
	sim := StringSimilarity(expected.Title, titleToCompare)

	// Special handling for symbol-only titles (e.g. "?" by MF DOOM, "+" by Sam The Kid, "$$$")
	if cand.TrackNumber > 0 && cand.TrackNumber == expected.TrackNumber {
		nExp := normalizeString(expected.Title)
		nCand := normalizeString(titleToCompare)
		if nExp == "" && (nCand == "" || titleToCompare == "_" || titleToCompare == expected.Title) {
			sim = 1.0
		}
	}
	score += 0.40 * sim

	// 3. Duration Matching (0.20 weight)
	durationDiff := 0
	if cand.DurationSec > 0 && expected.DurationSec > 0 {
		durationDiff = int(math.Abs(float64(cand.DurationSec - expected.DurationSec)))
		if durationDiff <= v.cfg.DurationToleranceSec {
			score += 0.20
		} else if durationDiff <= v.cfg.DurationToleranceSec*2 {
			score += 0.10
		}
	} else {
		// If duration is unknown, distribute neutral partial credit if title is a strong match or track matches
		if sim >= 0.85 || (sim >= 0.70 && cand.TrackNumber == expected.TrackNumber) {
			score += 0.15
		}
	}

	if score > 1.0 {
		score = 1.0
	}
	return score, durationDiff
}

// parseFilename attempts to extract track number, disc, and title from a clean filename.
func parseFilename(name string) (disc int, trackNum int, title string) {
	name = strings.TrimSpace(name)

	// Check for "CD 1 - 02 Title" or "Disc 1 - 02 Title"
	if matches := trackWithDiscPrefixRegex.FindStringSubmatch(name); len(matches) == 4 {
		disc, _ = strconv.Atoi(matches[1])
		trackNum, _ = strconv.Atoi(matches[2])
		title = strings.TrimSpace(matches[3])
		return
	}

	// Check for "Disc-Track Title", e.g. "1-02 Around the World"
	if matches := trackWithDiscRegex.FindStringSubmatch(name); len(matches) == 4 {
		disc, _ = strconv.Atoi(matches[1])
		trackNum, _ = strconv.Atoi(matches[2])
		title = strings.TrimSpace(matches[3])
		return
	}

	// Check for "Track - Title", e.g. "01 - Around the World" or "04 Digital Love"
	if matches := trackSimpleRegex.FindStringSubmatch(name); len(matches) == 3 {
		disc = 1
		trackNum, _ = strconv.Atoi(matches[1])
		title = strings.TrimSpace(matches[2])
		return
	}

	// Check for a number after a leading artist/album prefix, e.g. "Tame Impala - 01 - Let It Happen"
	if matches := trackEmbeddedRegex.FindStringSubmatch(name); len(matches) == 4 {
		disc = 1
		if matches[1] != "" {
			disc, _ = strconv.Atoi(matches[1])
		}
		trackNum, _ = strconv.Atoi(matches[2])
		title = strings.TrimSpace(matches[3])
		return
	}

	return 1, 0, name
}

func sanitizeFilename(name string) string {
	invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	res := name
	for _, c := range invalidChars {
		res = strings.ReplaceAll(res, c, "_")
	}
	return strings.TrimSpace(res)
}

// StringSimilarity calculates normalized string similarity between 0.0 and 1.0.
func StringSimilarity(s1, s2 string) float64 {
	t1 := strings.TrimSpace(s1)
	t2 := strings.TrimSpace(s2)

	if strings.EqualFold(t1, t2) {
		return 1.0
	}
	if sanitizeFilename(t1) == t2 || sanitizeFilename(t2) == t1 || strings.EqualFold(sanitizeFilename(t1), t2) {
		return 1.0
	}

	n1 := normalizeString(s1)
	n2 := normalizeString(s2)

	if n1 == "" || n2 == "" {
		// If both are punctuation/symbols only (e.g. "?", "+", "$$$")
		if n1 == "" && n2 == "" && t1 != "" && t2 != "" {
			if strings.EqualFold(t1, t2) || sanitizeFilename(t1) == t2 || sanitizeFilename(t2) == t1 {
				return 1.0
			}
			if t1 == "_" || t2 == "_" {
				return 1.0
			}
		}
		return 0.0
	}
	if n1 == n2 {
		return 1.0
	}

	// If one string contains the other (e.g. title with feat/remix vs base title)
	if strings.Contains(n1, n2) || strings.Contains(n2, n1) {
		shorter := len(n1)
		longer := len(n2)
		if shorter > longer {
			shorter, longer = longer, shorter
		}
		// Base coverage score between 0.80 and 1.0
		return 0.80 + 0.20*(float64(shorter)/float64(longer))
	}

	dist := levenshteinDistance([]rune(n1), []rune(n2))
	maxLen := len([]rune(n1))
	if len([]rune(n2)) > maxLen {
		maxLen = len([]rune(n2))
	}

	return 1.0 - (float64(dist) / float64(maxLen))
}

func normalizeString(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func levenshteinDistance(s1, s2 []rune) int {
	len1 := len(s1)
	len2 := len(s2)

	column := make([]int, len1+1)
	for y := 1; y <= len1; y++ {
		column[y] = y
	}

	for x := 1; x <= len2; x++ {
		column[0] = x
		lastkey := x - 1
		for y := 1; y <= len1; y++ {
			oldkey := column[y]
			cost := 0
			if s1[y-1] != s2[x-1] {
				cost = 1
			}
			column[y] = min(min(column[y]+1, column[y-1]+1), lastkey+cost)
			lastkey = oldkey
		}
	}

	return column[len1]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
