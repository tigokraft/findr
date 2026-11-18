package links

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in             string
		platform, kind string
		id             string
	}{
		{"https://www.deezer.com/en/album/302127", Deezer, "album", "302127"},
		{"deezer.com/artist/27", Deezer, "artist", "27"},
		{"https://www.deezer.com/track/3135556?utm=x", Deezer, "track", "3135556"},
		{"https://deezer.page.link/abc123", Deezer, "short", ""},
		{"https://open.qobuz.com/album/0060254728451", Qobuz, "album", "0060254728451"},
		{"https://play.qobuz.com/artist/36819", Qobuz, "artist", "36819"},
		{"https://www.qobuz.com/gb-en/album/discovery-daft-punk/0724384960650", Qobuz, "album", "0724384960650"},
		{"https://www.qobuz.com/us-en/interpreter/daft-punk/36819", Qobuz, "artist", "36819"},
		{"https://open.spotify.com/intl-pt/artist/4tZwfgrHOc3mvqYlEYSvVi?si=1", Spotify, "artist", "4tZwfgrHOc3mvqYlEYSvVi"},
		{"https://open.spotify.com/album/2noRn2Aes5aoNVsU6iWThc", Spotify, "album", "2noRn2Aes5aoNVsU6iWThc"},
		{"spotify:track:0DiWol3AO6WpXZgp0goxAV", Spotify, "track", "0DiWol3AO6WpXZgp0goxAV"},
		{"https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M", Spotify, "playlist", "37i9dQZF1DXcBWIGoYBM5M"},
		{"https://music.apple.com/us/album/discovery/697194953", AppleMusic, "album", "697194953"},
		{"https://music.apple.com/us/album/one-more-time/697194953?i=697195462", AppleMusic, "track", "697195462"},
		{"https://music.apple.com/us/artist/daft-punk/5468295", AppleMusic, "artist", "5468295"},
		{"https://tidal.com/browse/album/1234567", Tidal, "album", "1234567"},
		{"https://listen.tidal.com/track/7654321", Tidal, "track", "7654321"},
		{"https://music.youtube.com/channel/UC_kRDKYrUlrbtrSiyu5Tflg", YouTubeMusic, "artist", "UC_kRDKYrUlrbtrSiyu5Tflg"},
		{"https://music.youtube.com/watch?v=FGBhQbmPwH8", YouTubeMusic, "track", "FGBhQbmPwH8"},
		{"https://music.youtube.com/playlist?list=OLAK5uy_abc", YouTubeMusic, "album", "OLAK5uy_abc"},
		{"https://youtu.be/FGBhQbmPwH8", YouTube, "track", "FGBhQbmPwH8"},
		{"https://soundcloud.com/daftpunkofficialmusic", SoundCloud, "artist", "daftpunkofficialmusic"},
		{"https://soundcloud.com/artist/some-track", SoundCloud, "track", "artist/some-track"},
		{"https://soundcloud.com/artist/sets/an-ep", SoundCloud, "album", "artist/sets/an-ep"},
		{"https://someband.bandcamp.com/album/first-record", Bandcamp, "album", "someband/album/first-record"},
		{"https://someband.bandcamp.com/", Bandcamp, "artist", "someband/"},
		{"https://music.amazon.com/albums/B00123?trackAsin=B00456", AmazonMusic, "track", "B00456"},
	}
	for _, c := range cases {
		ref, err := Parse(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if ref.Platform != c.platform || string(ref.Kind) != c.kind || (c.id != "" && ref.ID != c.id) {
			t.Errorf("%s: got %s/%s/%s, want %s/%s/%s", c.in, ref.Platform, ref.Kind, ref.ID, c.platform, c.kind, c.id)
		}
	}
	for _, bad := range []string{"Daft Punk", "https://example.com/album/1", "https://open.spotify.com/user/abc"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestLooksLikeLink(t *testing.T) {
	if LooksLikeLink("Daft Punk") || LooksLikeLink("discovery") {
		t.Error("plain text is not a link")
	}
	if !LooksLikeLink("open.spotify.com/artist/4tZwfgrHOc3mvqYlEYSvVi") || !LooksLikeLink("https://anything.example") {
		t.Error("links should be detected")
	}
}

func TestParsePageMeta(t *testing.T) {
	spotify := &Ref{Platform: Spotify, Kind: KindAlbum}
	page := `<html><head><meta property="og:title" content="Listen to Discovery - Album by Daft Punk | Spotify"><meta property="og:description" content="Daft Punk · album · 2001 · 14 songs"></head></html>`
	if m := ParsePageMeta(spotify, page); m.Title != "Discovery" || m.Artist != "Daft Punk" {
		t.Errorf("spotify album: %+v", m)
	}
	spTrack := &Ref{Platform: Spotify, Kind: KindTrack}
	page = `<meta property="og:title" content="Stand by Me"/><meta property="og:description" content="Ben E. King · Don't Play That Song · Song · 1962"/><title>Stand by Me - song and lyrics by Ben E. King | Spotify</title>`
	if m := ParsePageMeta(spTrack, page); m.Title != "Stand by Me" || m.Artist != "Ben E. King" {
		t.Errorf("spotify track with 'by' in title: %+v", m)
	}
	bc := &Ref{Platform: Bandcamp, Kind: KindAlbum}
	if m := ParsePageMeta(bc, `<meta property="og:title" content="First Record, by Some Band">`); m.Title != "First Record" || m.Artist != "Some Band" {
		t.Errorf("bandcamp album: %+v", m)
	}
	tidal := &Ref{Platform: Tidal, Kind: KindTrack}
	page = `<meta property="og:title" content="Michael Jackson - Billie Jean"><meta property="og:description" content="Listen to Billie Jean on TIDAL"><title>Billie Jean by Michael Jackson on TIDAL</title>`
	if m := ParsePageMeta(tidal, page); m.Title != "Billie Jean" || m.Artist != "Michael Jackson" {
		t.Errorf("tidal track: %+v", m)
	}
	art := &Ref{Platform: Spotify, Kind: KindArtist}
	if m := ParsePageMeta(art, `<title>Daft Punk | Spotify</title>`); m.Artist != "Daft Punk" {
		t.Errorf("artist from <title>: %+v", m)
	}
	yt := &Ref{Platform: YouTubeMusic, Kind: KindTrack}
	if m := ParsePageMeta(yt, `<meta property="og:title" content="One More Time"><meta property="og:description" content="Daft Punk"><title>One More Time</title>`); m.Title != "One More Time" || m.Artist != "Daft Punk" {
		t.Errorf("youtube music: %+v", m)
	}
	if ref, _ := Parse("https://www.qobuz.com/gb-en/album/discovery-daft-punk/0724384960650"); ref.Slug != "discovery daft punk" {
		t.Errorf("qobuz slug: %q", ref.Slug)
	}
}
