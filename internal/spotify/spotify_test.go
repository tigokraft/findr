package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testID = "37i9dQZF1DXcBWIGoYBM5M"

func TestParsePlaylistID(t *testing.T) {
	ok := []string{
		testID,
		"https://open.spotify.com/playlist/" + testID,
		"https://open.spotify.com/playlist/" + testID + "?si=abc123",
		"https://open.spotify.com/intl-pt/playlist/" + testID,
		"spotify:playlist:" + testID,
		"  https://open.spotify.com/embed/playlist/" + testID + "  ",
	}
	for _, in := range ok {
		got, err := ParsePlaylistID(in)
		if err != nil || got != testID {
			t.Errorf("ParsePlaylistID(%q) = %q, %v", in, got, err)
		}
	}
	bad := []string{"", "hello", "https://example.com/playlist/" + testID, "https://open.spotify.com/album/" + testID}
	for _, in := range bad {
		if _, err := ParsePlaylistID(in); err == nil {
			t.Errorf("ParsePlaylistID(%q) should fail", in)
		}
	}
}

func embedPage(n int) string {
	var tracks []string
	for i := 0; i < n; i++ {
		tracks = append(tracks, fmt.Sprintf(`{"uri":"spotify:track:%d","title":"Song %d","subtitle":"Artist A, Artist B","duration":225834,"entityType":"track"}`, i, i))
	}
	data := `{"props":{"pageProps":{"state":{"data":{"entity":{"name":"Mix","subtitle":"Someone","coverArt":{"sources":[{"url":"https://img/cover"}]},"trackList":[` + strings.Join(tracks, ",") + `]}}}}}}`
	return `<html><script id="__NEXT_DATA__" type="application/json">` + data + `</script></html>`
}

func TestParseEmbed(t *testing.T) {
	pl, err := parseEmbed(testID, []byte(embedPage(3)))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Name != "Mix" || pl.Owner != "Someone" || pl.ImageURL != "https://img/cover" || pl.Via != "embed" || pl.Partial {
		t.Fatalf("bad playlist meta: %+v", pl)
	}
	if len(pl.Tracks) != 3 || pl.Tracks[0].DurationSec != 225 {
		t.Fatalf("bad tracks: %+v", pl.Tracks)
	}
	if got := pl.Tracks[0].Artists; len(got) != 2 || got[0] != "Artist A" || got[1] != "Artist B" {
		t.Fatalf("artists not split: %q", got)
	}
	full, _ := parseEmbed(testID, []byte(embedPage(100)))
	if !full.Partial {
		t.Fatal("100 embed tracks should be flagged partial")
	}
	if _, err := parseEmbed(testID, []byte("<html>nothing</html>")); err == nil {
		t.Fatal("missing data should error")
	}
}

// fakeSpotify serves the token, playlist and embed endpoints.
func fakeSpotify(t *testing.T, itemsStatus int) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "id" || p != "secret" {
			http.Error(w, "bad auth", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "tok", "expires_in": 3600})
	})
	mux.HandleFunc("/v1/playlists/"+testID, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"API Mix","owner":{"display_name":"Me"},"images":[{"url":"https://img/api"}]}`))
	})
	mux.HandleFunc("/v1/playlists/"+testID+"/items", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		if itemsStatus != 200 {
			http.Error(w, "forbidden", itemsStatus)
			return
		}
		if r.URL.Query().Get("offset") == "0" {
			_, _ = w.Write([]byte(`{"items":[{"item":{"type":"track","name":"One","duration_ms":200000,"external_ids":{"isrc":"usabc1234567"},"artists":[{"name":"X"}],"album":{"name":"LP"}}},{"track":{"type":"episode","name":"Pod"}}],"next":"more"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"track":{"type":"track","name":"Two","duration_ms":100000,"artists":[{"name":"Y"}]}}],"next":null}`))
	})
	mux.HandleFunc("/v1/playlists/"+testID+"/tracks", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusForbidden)
	})
	mux.HandleFunc("/embed/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(embedPage(2)))
	})
	return httptest.NewServer(mux)
}

func newTestClient(srv *httptest.Server, id, secret string) *Client {
	c := NewClient(id, secret)
	c.apiBase = srv.URL + "/v1"
	c.tokenURL = srv.URL + "/token"
	c.embedBase = srv.URL + "/embed/"
	return c
}

func TestGetPlaylistViaAPI(t *testing.T) {
	srv := fakeSpotify(t, 200)
	defer srv.Close()
	pl, err := newTestClient(srv, "id", "secret").GetPlaylist(context.Background(), testID)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Via != "api" || pl.Name != "API Mix" || len(pl.Tracks) != 2 {
		t.Fatalf("unexpected: %+v", pl)
	}
	if pl.Tracks[0].ISRC != "USABC1234567" || pl.Tracks[0].Album != "LP" || pl.Tracks[1].Title != "Two" {
		t.Fatalf("bad tracks: %+v", pl.Tracks)
	}
}

func TestGetPlaylistFallsBackToEmbed(t *testing.T) {
	srv := fakeSpotify(t, http.StatusForbidden)
	defer srv.Close()
	pl, err := newTestClient(srv, "id", "secret").GetPlaylist(context.Background(), testID)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Via != "embed" || len(pl.Tracks) != 2 {
		t.Fatalf("expected embed fallback, got %+v", pl)
	}
	// No credentials: straight to embed.
	pl, err = newTestClient(srv, "", "").GetPlaylist(context.Background(), testID)
	if err != nil || pl.Via != "embed" {
		t.Fatalf("no-cred path: %+v %v", pl, err)
	}
}
