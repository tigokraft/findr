// Package links recognises music links from streaming platforms and looks up
// what they point to (artist name, release title) so findr can find the same
// thing in its own catalogs.
package links

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Kind is what a link points to.
type Kind string

const (
	KindArtist   Kind = "artist"
	KindAlbum    Kind = "album"
	KindTrack    Kind = "track"
	KindPlaylist Kind = "playlist"
	// KindShort is a short/share link that must be followed to learn the rest.
	KindShort Kind = "short"
)

// Platform identifiers.
const (
	Deezer       = "deezer"
	Qobuz        = "qobuz"
	Spotify      = "spotify"
	AppleMusic   = "apple_music"
	Tidal        = "tidal"
	YouTubeMusic = "youtube_music"
	YouTube      = "youtube"
	SoundCloud   = "soundcloud"
	Bandcamp     = "bandcamp"
	AmazonMusic  = "amazon_music"
)

// PlatformNames maps platform IDs to display names.
var PlatformNames = map[string]string{
	Deezer: "Deezer", Qobuz: "Qobuz", Spotify: "Spotify", AppleMusic: "Apple Music",
	Tidal: "TIDAL", YouTubeMusic: "YouTube Music", YouTube: "YouTube",
	SoundCloud: "SoundCloud", Bandcamp: "Bandcamp", AmazonMusic: "Amazon Music",
}

// Ref is a parsed music link.
type Ref struct {
	Platform string `json:"platform"`
	Kind     Kind   `json:"kind"`
	ID       string `json:"id,omitempty"`
	URL      string `json:"url"`
	// Slug is the human-readable part of the URL when it names the item
	// (Qobuz store links: "discovery-daft-punk"), used when nothing else works.
	Slug string `json:"slug,omitempty"`
}

var (
	deezerPathRe  = regexp.MustCompile(`^/(?:[a-z]{2}(?:-[a-z]{2})?/)?(artist|album|track|playlist)/(\d+)`)
	qobuzOpenRe   = regexp.MustCompile(`^/(artist|album|track|playlist)/([A-Za-z0-9]+)`)
	qobuzStoreRe  = regexp.MustCompile(`^/[a-z]{2}-[a-z]{2}/(album|interpreter|playlists)/([^/]+)/([A-Za-z0-9]+)`)
	spotifyPathRe = regexp.MustCompile(`^/(?:intl-[a-z]{2}(?:-[a-z]{2})?/|embed/)?(artist|album|track|playlist)/([A-Za-z0-9]{22})`)
	spotifyURIRe  = regexp.MustCompile(`^spotify:(?:user:[^:]+:)?(artist|album|track|playlist):([A-Za-z0-9]{22})$`)
	appleRe       = regexp.MustCompile(`^/[a-z]{2}/(artist|album|song|playlist|music-video)/(?:[^/]+/)?([A-Za-z0-9.\-]+)$`)
	tidalRe       = regexp.MustCompile(`^/(?:browse/)?(artist|album|track|playlist|mix)/([A-Za-z0-9\-]+)`)
	amazonRe      = regexp.MustCompile(`^/(artists|albums|tracks|playlists|user-playlists)/([A-Za-z0-9]+)`)
)

// Parse recognises a link. It returns an error for text that is not a URL or
// for a site findr does not know.
func Parse(raw string) (*Ref, error) {
	s := strings.TrimSpace(raw)
	if m := spotifyURIRe.FindStringSubmatch(s); m != nil {
		return &Ref{Platform: Spotify, Kind: Kind(m[1]), ID: m[2], URL: "https://open.spotify.com/" + m[1] + "/" + m[2]}, nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, ".") {
		return nil, fmt.Errorf("not a link: %q", raw)
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	path := strings.TrimRight(u.EscapedPath(), "/")
	ref := &Ref{URL: u.String()}

	switch {
	case host == "deezer.com":
		ref.Platform = Deezer
		if m := deezerPathRe.FindStringSubmatch(path); m != nil {
			ref.Kind, ref.ID = Kind(m[1]), m[2]
			return ref, nil
		}
	case host == "deezer.page.link" || host == "link.deezer.com" || host == "dzr.page.link":
		ref.Platform, ref.Kind = Deezer, KindShort
		return ref, nil

	case host == "open.qobuz.com" || host == "play.qobuz.com":
		ref.Platform = Qobuz
		if m := qobuzOpenRe.FindStringSubmatch(path); m != nil {
			ref.Kind, ref.ID = Kind(m[1]), m[2]
			return ref, nil
		}
	case host == "qobuz.com":
		ref.Platform = Qobuz
		if m := qobuzStoreRe.FindStringSubmatch(path); m != nil {
			switch m[1] {
			case "interpreter":
				ref.Kind = KindArtist
			case "playlists":
				ref.Kind = KindPlaylist
			default:
				ref.Kind = KindAlbum
			}
			ref.ID = m[3]
			if slug, err := url.PathUnescape(m[2]); err == nil {
				ref.Slug = strings.ReplaceAll(slug, "-", " ")
			}
			return ref, nil
		}

	case host == "open.spotify.com" || host == "play.spotify.com":
		ref.Platform = Spotify
		if m := spotifyPathRe.FindStringSubmatch(path); m != nil {
			ref.Kind, ref.ID = Kind(m[1]), m[2]
			ref.URL = "https://open.spotify.com/" + m[1] + "/" + m[2]
			return ref, nil
		}
	case host == "spotify.link" || host == "spotify.app.link":
		ref.Platform, ref.Kind = Spotify, KindShort
		return ref, nil

	case host == "music.apple.com" || host == "itunes.apple.com" || host == "geo.music.apple.com":
		ref.Platform = AppleMusic
		if m := appleRe.FindStringSubmatch(path); m != nil {
			ref.ID = m[2]
			switch m[1] {
			case "song", "music-video":
				ref.Kind = KindTrack
			default:
				ref.Kind = Kind(m[1])
			}
			// An album link with ?i= points at one of its tracks.
			if ref.Kind == KindAlbum && u.Query().Get("i") != "" {
				ref.Kind, ref.ID = KindTrack, u.Query().Get("i")
			}
			return ref, nil
		}

	case host == "tidal.com" || host == "listen.tidal.com":
		ref.Platform = Tidal
		if m := tidalRe.FindStringSubmatch(path); m != nil {
			ref.Kind, ref.ID = Kind(m[1]), m[2]
			if m[1] == "mix" {
				ref.Kind = KindPlaylist
			}
			return ref, nil
		}

	case host == "music.youtube.com":
		ref.Platform = YouTubeMusic
		switch {
		case strings.HasPrefix(path, "/channel/"):
			ref.Kind, ref.ID = KindArtist, strings.TrimPrefix(path, "/channel/")
			return ref, nil
		case strings.HasPrefix(path, "/browse/MPRE"):
			ref.Kind, ref.ID = KindAlbum, strings.TrimPrefix(path, "/browse/")
			return ref, nil
		case path == "/watch" && u.Query().Get("v") != "":
			ref.Kind, ref.ID = KindTrack, u.Query().Get("v")
			return ref, nil
		case path == "/playlist" && strings.HasPrefix(u.Query().Get("list"), "OLAK5uy_"):
			ref.Kind, ref.ID = KindAlbum, u.Query().Get("list") // auto-generated album playlist
			return ref, nil
		case path == "/playlist" && u.Query().Get("list") != "":
			ref.Kind, ref.ID = KindPlaylist, u.Query().Get("list")
			return ref, nil
		}
	case host == "youtube.com" || host == "m.youtube.com" || host == "youtu.be":
		ref.Platform = YouTube
		if host == "youtu.be" && len(path) > 1 {
			ref.Kind, ref.ID = KindTrack, strings.TrimPrefix(path, "/")
			return ref, nil
		}
		if path == "/watch" && u.Query().Get("v") != "" {
			ref.Kind, ref.ID = KindTrack, u.Query().Get("v")
			return ref, nil
		}

	case host == "soundcloud.com" || host == "m.soundcloud.com" || host == "on.soundcloud.com":
		ref.Platform = SoundCloud
		if host == "on.soundcloud.com" {
			ref.Kind = KindShort
			return ref, nil
		}
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case len(parts) == 1 && parts[0] != "":
			ref.Kind = KindArtist
		case len(parts) == 3 && parts[1] == "sets":
			ref.Kind = KindAlbum
		case len(parts) == 2 && parts[1] != "sets" && parts[1] != "tracks" && parts[1] != "albums":
			ref.Kind = KindTrack
		}
		if ref.Kind != "" {
			ref.ID = strings.Join(parts, "/")
			return ref, nil
		}

	case strings.HasSuffix(host, ".bandcamp.com"):
		ref.Platform = Bandcamp
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case len(parts) == 1 && (parts[0] == "" || parts[0] == "music"):
			ref.Kind = KindArtist
		case len(parts) == 2 && parts[0] == "album":
			ref.Kind = KindAlbum
		case len(parts) == 2 && parts[0] == "track":
			ref.Kind = KindTrack
		}
		if ref.Kind != "" {
			ref.ID = strings.TrimSuffix(host, ".bandcamp.com") + "/" + strings.Join(parts, "/")
			return ref, nil
		}

	case strings.HasPrefix(host, "music.amazon."):
		ref.Platform = AmazonMusic
		if m := amazonRe.FindStringSubmatch(path); m != nil {
			ref.ID = m[2]
			switch m[1] {
			case "artists":
				ref.Kind = KindArtist
			case "albums":
				ref.Kind = KindAlbum
				if t := u.Query().Get("trackAsin"); t != "" {
					ref.Kind, ref.ID = KindTrack, t
				}
			case "tracks":
				ref.Kind = KindTrack
			default:
				ref.Kind = KindPlaylist
			}
			return ref, nil
		}
	}

	if ref.Platform != "" {
		return nil, fmt.Errorf("this %s link does not point to an artist, album or track", PlatformNames[ref.Platform])
	}
	return nil, fmt.Errorf("links from %s are not supported", host)
}

// LooksLikeLink reports whether search input should be treated as a link.
func LooksLikeLink(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "spotify:") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return true
	}
	if strings.ContainsAny(s, " \t") {
		return false
	}
	_, err := Parse(s)
	return err == nil
}

// Meta is what a link's page says it is.
type Meta struct {
	Title  string `json:"title,omitempty"`  // release or track title
	Artist string `json:"artist,omitempty"` // artist name (for an artist link: the name)
	// Loose is set when only a free-text query is known (title and artist
	// mixed together in Title), e.g. from a URL slug.
	Loose bool `json:"loose,omitempty"`
}

// Fetcher looks up link metadata over HTTP.
type Fetcher struct {
	HTTP *http.Client
	// ITunesBase is Apple's public lookup API, overridable in tests.
	ITunesBase string
}

// NewFetcher returns a Fetcher with sane timeouts.
func NewFetcher() *Fetcher {
	return &Fetcher{
		HTTP:       &http.Client{Timeout: 12 * time.Second},
		ITunesBase: "https://itunes.apple.com/lookup",
	}
}

const userAgent = "Mozilla/5.0 (compatible; findr/1.0; +https://github.com/tigokraft/findr)"

func (f *Fetcher) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en")
	return f.HTTP.Do(req)
}

// Expand follows a short link and parses where it lands.
func (f *Fetcher) Expand(ctx context.Context, ref *Ref) (*Ref, error) {
	resp, err := f.get(ctx, ref.URL)
	if err != nil {
		return nil, fmt.Errorf("follow short link: %w", err)
	}
	defer resp.Body.Close()
	final := resp.Request.URL.String()
	out, err := Parse(final)
	if err == nil && out.Kind != KindShort {
		return out, nil
	}
	// Some share pages redirect with JavaScript; look for a canonical/og:url.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	for _, key := range []string{"og:url", "canonical"} {
		if u := metaContent(string(body), key); u != "" {
			if out, err := Parse(u); err == nil && out.Kind != KindShort {
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("could not tell where the short link %s points", ref.URL)
}

// Lookup finds the title/artist of what ref points to: Apple's public lookup
// API for Apple Music, otherwise the page's Open Graph tags, and as a last
// resort the words in the URL.
func (f *Fetcher) Lookup(ctx context.Context, ref *Ref) (*Meta, error) {
	var errs []string
	if ref.Platform == AppleMusic {
		m, err := f.itunes(ctx, ref)
		if err == nil {
			return m, nil
		}
		errs = append(errs, "Apple lookup: "+err.Error())
	}
	if ref.Slug == "" || ref.Platform != Qobuz { // Qobuz store pages refuse bots
		m, err := f.pageMeta(ctx, ref)
		if err == nil {
			return m, nil
		}
		errs = append(errs, "page: "+err.Error())
	}
	if ref.Slug != "" {
		if ref.Kind == KindArtist {
			return &Meta{Artist: ref.Slug}, nil
		}
		return &Meta{Title: ref.Slug, Loose: true}, nil
	}
	return nil, fmt.Errorf("could not read the %s link (%s)", PlatformNames[ref.Platform], strings.Join(errs, "; "))
}

func (f *Fetcher) itunes(ctx context.Context, ref *Ref) (*Meta, error) {
	resp, err := f.get(ctx, f.ITunesBase+"?id="+url.QueryEscape(ref.ID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Results []struct {
			WrapperType    string `json:"wrapperType"`
			ArtistName     string `json:"artistName"`
			CollectionName string `json:"collectionName"`
			TrackName      string `json:"trackName"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Results) == 0 {
		return nil, fmt.Errorf("no result for id %s", ref.ID)
	}
	r := body.Results[0]
	m := &Meta{Artist: r.ArtistName}
	switch ref.Kind {
	case KindAlbum:
		m.Title = r.CollectionName
	case KindTrack:
		m.Title = r.TrackName
	}
	return m, nil
}

func (f *Fetcher) pageMeta(ctx context.Context, ref *Ref) (*Meta, error) {
	resp, err := f.get(ctx, ref.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	if err != nil {
		return nil, err
	}
	m := ParsePageMeta(ref, string(body))
	if m.Artist == "" && m.Title == "" {
		return nil, fmt.Errorf("the page has no title")
	}
	return m, nil
}

var (
	metaTagRe = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	attrRe    = regexp.MustCompile(`(?is)(property|name|content|rel|href)\s*=\s*("([^"]*)"|'([^']*)')`)
	linkTagRe = regexp.MustCompile(`(?is)<link\s+[^>]*>`)
	titleRe   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// metaContent returns the content of <meta property|name=key> (or the href of
// <link rel=key>), HTML-unescaped.
func metaContent(page, key string) string {
	for _, tag := range metaTagRe.FindAllString(page, -1) {
		attrs := tagAttrs(tag)
		if strings.EqualFold(attrs["property"], key) || strings.EqualFold(attrs["name"], key) {
			return strings.TrimSpace(html.UnescapeString(attrs["content"]))
		}
	}
	for _, tag := range linkTagRe.FindAllString(page, -1) {
		attrs := tagAttrs(tag)
		if strings.EqualFold(attrs["rel"], key) {
			return strings.TrimSpace(html.UnescapeString(attrs["href"]))
		}
	}
	return ""
}

func tagAttrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		v := m[3]
		if v == "" {
			v = m[4]
		}
		out[strings.ToLower(m[1])] = v
	}
	return out
}

// Site suffixes and prefixes that pages wrap their titles in.
var (
	titleSuffixRe = regexp.MustCompile(`(?i)\s*(?:[|\-–—]\s*(?:spotify|youtube music|youtube|tidal|bandcamp|amazon music|apple music|deezer|qobuz)|on (?:apple music|tidal|amazon music|spotify|deezer|youtube music)|[|\-–—]\s*(?:free listening|listen|stream)[^|]*on soundcloud|on soundcloud)\s*$`)
	titlePrefixRe = regexp.MustCompile(`(?i)^(?:listen to|stream|play)\s+`)
	kindSuffixRe  = regexp.MustCompile(`(?i)\s+-\s+(?:album|single|ep|song and lyrics|song|songs)$`)
	// Greedy title: the last " by " splits it, so "Stand by Me by Ben E. King" works.
	byRe = regexp.MustCompile(`(?i)^(.+)\s+by\s+(.+)$`)
)

func cleanTitle(t string) string {
	t = strings.TrimSpace(t)
	for i := 0; i < 2; i++ {
		t = strings.TrimSpace(titleSuffixRe.ReplaceAllString(t, ""))
	}
	return strings.TrimSpace(titlePrefixRe.ReplaceAllString(t, ""))
}

// ParsePageMeta reads title/artist out of a page's Open Graph tags and <title>.
func ParsePageMeta(ref *Ref, page string) *Meta {
	ogRaw := metaContent(page, "og:title")
	docRaw := ""
	if m := titleRe.FindStringSubmatch(page); m != nil {
		docRaw = strings.TrimSpace(html.UnescapeString(m[1]))
	}
	og, doc := cleanTitle(ogRaw), cleanTitle(docRaw)
	desc := metaContent(page, "og:description")

	m := &Meta{}
	if ref.Kind == KindArtist {
		m.Artist = og
		if m.Artist == "" {
			m.Artist = doc
		}
		return m
	}
	// Spotify tracks: og:title is the bare title and the description starts
	// with the artist ("Daft Punk · Discovery · Song · 2001").
	if ref.Platform == Spotify && og != "" && og == strings.TrimSpace(ogRaw) && strings.Contains(desc, " · ") {
		m.Title = og
		m.Artist = strings.TrimSpace(strings.Split(desc, " · ")[0])
		return m
	}
	// "Title by Artist" is reliable only where the site wrapped the title
	// ("… on TIDAL", "Listen to … | Spotify", Bandcamp's "Title, by Artist");
	// a bare title may contain " by " itself.
	for _, c := range [][2]string{{og, ogRaw}, {doc, docRaw}} {
		clean, raw := c[0], c[1]
		if clean == strings.TrimSpace(raw) && !strings.Contains(clean, ", by ") {
			continue
		}
		if b := byRe.FindStringSubmatch(clean); b != nil {
			title := strings.TrimRight(strings.TrimSpace(b[1]), ", ")
			m.Title = strings.TrimSpace(kindSuffixRe.ReplaceAllString(title, ""))
			m.Artist = strings.TrimSpace(b[2])
			return m
		}
	}
	m.Title = og
	if m.Title == "" {
		m.Title = doc
	}
	switch ref.Platform {
	case Spotify:
		// "Daft Punk · Album · 2001 · 14 songs" / "Daft Punk · Discovery · Song · 2001"
		if strings.Contains(desc, " · ") {
			m.Artist = strings.TrimSpace(strings.Split(desc, " · ")[0])
		}
	case YouTubeMusic:
		// og:description is the artist on track pages.
		if desc != "" && len(desc) < 120 && !strings.Contains(strings.ToLower(desc), "youtube") {
			m.Artist = desc
		}
	}
	// "Artist - Title" (TIDAL og:title, YouTube video titles).
	if m.Artist == "" {
		if parts := strings.SplitN(m.Title, " - ", 2); len(parts) == 2 {
			m.Artist, m.Title = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		}
	}
	m.Title = strings.TrimSpace(kindSuffixRe.ReplaceAllString(m.Title, ""))
	if m.Artist == "" {
		m.Loose = true
	}
	return m
}
