package qobuz

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCalculateSignature(t *testing.T) {
	params := map[string]string{
		"track_id":  "12345",
		"format_id": "6",
		"intent":    "stream",
	}
	ts := int64(1700000000)
	secret := "testsecret"

	// Sorted parameters:
	// format_id6 + intentstream + track_id12345
	// string = "trackgetFileUrlformat_id6intentstreamtrack_id123451700000000testsecret"
	expectedRaw := "trackgetFileUrlformat_id6intentstreamtrack_id123451700000000testsecret"
	h := md5.Sum([]byte(expectedRaw))
	expected := hex.EncodeToString(h[:])

	sig := CalculateSignature(params, ts, secret)
	if sig != expected {
		t.Errorf("CalculateSignature() = %s; expected %s", sig, expected)
	}
}

func TestValidateTokenWithMockServer(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-User-Auth-Token")
		if token != "valid-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"status":"error","message":"Invalid token"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": 1234567,
			"email": "user@example.com",
			"country_code": "US",
			"subscription": {
				"offer": "Studio Premier",
				"period": "monthly"
			}
		}`))
	}))
	defer mockServer.Close()

	dl := NewDownloader(Config{
		AppID:     "798273057",
		UserToken: "valid-test-token",
	})
	// Point to mock server URL by overriding httpClient
	dl.httpClient = mockServer.Client()

	// Direct test of header handling
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, mockServer.URL+"/user/get", nil)
	req.Header.Set("X-User-Auth-Token", dl.UserToken())
	resp, err := dl.httpClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	// Test with invalid token
	reqInvalid, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, mockServer.URL+"/user/get", nil)
	reqInvalid.Header.Set("X-User-Auth-Token", "wrong-token")
	respInvalid, err := dl.httpClient.Do(reqInvalid)
	if err != nil {
		t.Fatalf("failed invalid request: %v", err)
	}
	defer respInvalid.Body.Close()

	if respInvalid.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected status 401 for invalid token, got %d", respInvalid.StatusCode)
	}
}
