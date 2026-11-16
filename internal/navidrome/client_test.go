package navidrome

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHashToken(t *testing.T) {
	password := "secret123"
	salt := "randomsalt"

	expectedHash := md5.Sum([]byte(password + salt))
	expected := hex.EncodeToString(expectedHash[:])

	actual := hashToken(password, salt)
	if actual != expected {
		t.Errorf("hashToken() = %s; expected %s", actual, expected)
	}
}

func TestNavidromePing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/ping" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"subsonic-response": {"status": "ok", "version": "1.16.0"}}`))
	}))
	defer ts.Close()

	client := NewClient(Config{
		URL:       ts.URL,
		Username:  "admin",
		Password:  "password",
		TokenAuth: true,
	})

	err := client.Ping(context.Background())
	if err != nil {
		t.Fatalf("expected ping to succeed, got %v", err)
	}
}
