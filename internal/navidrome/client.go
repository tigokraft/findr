package navidrome

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	subsonicAPIVersion = "1.16.0"
	clientName         = "findr"
)

// Config configures the Navidrome Subsonic client.
type Config struct {
	URL       string
	Username  string
	Password  string
	TokenAuth bool
}

// Client interacts with Navidrome via the Subsonic REST API.
type Client struct {
	baseURL    string
	username   string
	password   string
	tokenAuth  bool
	httpClient *http.Client
}

// NewClient initializes a new Navidrome client.
func NewClient(cfg Config) *Client {
	baseURL := strings.TrimRight(cfg.URL, "/")
	return &Client{
		baseURL:    baseURL,
		username:   cfg.Username,
		password:   cfg.Password,
		tokenAuth:  cfg.TokenAuth,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// ScanStatus represents the progress or completion of a library scan.
type ScanStatus struct {
	Scanning bool  `json:"scanning"`
	Count    int64 `json:"count"`
}

type subsonicResponse struct {
	SubsonicResponse struct {
		Status     string     `json:"status"` // "ok" or "failed"
		Version    string     `json:"version"`
		ScanStatus ScanStatus `json:"scanStatus"`
		Error      *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	} `json:"subsonic-response"`
}

// StartScan triggers Navidrome to scan the music library for newly imported files.
func (c *Client) StartScan(ctx context.Context, fullScan bool) (*ScanStatus, error) {
	params := c.buildAuthParams()
	if fullScan {
		params.Set("fullScan", "true")
	}

	reqURL := fmt.Sprintf("%s/rest/startScan?%s", c.baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("navidrome startScan request failed: %w", err)
	}
	defer resp.Body.Close()

	var subResp subsonicResponse
	if err := json.NewDecoder(resp.Body).Decode(&subResp); err != nil {
		return nil, fmt.Errorf("decode navidrome startScan response: %w", err)
	}

	if subResp.SubsonicResponse.Status != "ok" {
		errMsg := "unknown error"
		if subResp.SubsonicResponse.Error != nil {
			errMsg = subResp.SubsonicResponse.Error.Message
		}
		return nil, fmt.Errorf("navidrome error: %s", errMsg)
	}

	return &subResp.SubsonicResponse.ScanStatus, nil
}

// Ping tests the connection and credentials to the Navidrome server.
func (c *Client) Ping(ctx context.Context) error {
	params := c.buildAuthParams()
	reqURL := fmt.Sprintf("%s/rest/ping?%s", c.baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("navidrome connection failed: %w", err)
	}
	defer resp.Body.Close()

	var subResp subsonicResponse
	if err := json.NewDecoder(resp.Body).Decode(&subResp); err != nil {
		return fmt.Errorf("decode navidrome ping response: %w", err)
	}

	if subResp.SubsonicResponse.Status != "ok" {
		errMsg := "authentication or server error"
		if subResp.SubsonicResponse.Error != nil {
			errMsg = subResp.SubsonicResponse.Error.Message
		}
		return fmt.Errorf("navidrome error: %s", errMsg)
	}

	return nil
}

// GetScanStatus checks whether Navidrome is currently scanning and how many tracks are indexed.
func (c *Client) GetScanStatus(ctx context.Context) (*ScanStatus, error) {
	params := c.buildAuthParams()
	reqURL := fmt.Sprintf("%s/rest/getScanStatus?%s", c.baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("navidrome getScanStatus request failed: %w", err)
	}
	defer resp.Body.Close()

	var subResp subsonicResponse
	if err := json.NewDecoder(resp.Body).Decode(&subResp); err != nil {
		return nil, fmt.Errorf("decode navidrome getScanStatus response: %w", err)
	}

	if subResp.SubsonicResponse.Status != "ok" {
		errMsg := "unknown error"
		if subResp.SubsonicResponse.Error != nil {
			errMsg = subResp.SubsonicResponse.Error.Message
		}
		return nil, fmt.Errorf("navidrome error: %s", errMsg)
	}

	return &subResp.SubsonicResponse.ScanStatus, nil
}

// buildAuthParams constructs Subsonic URL query parameters with salt + token hashing.
func (c *Client) buildAuthParams() url.Values {
	params := url.Values{}
	params.Set("u", c.username)
	params.Set("v", subsonicAPIVersion)
	params.Set("c", clientName)
	params.Set("f", "json")

	if c.tokenAuth {
		salt := generateSalt(8)
		token := hashToken(c.password, salt)
		params.Set("s", salt)
		params.Set("t", token)
	} else {
		params.Set("p", c.password)
	}

	return params
}

// hashToken generates md5(password + salt) as specified by Subsonic API.
func hashToken(password, salt string) string {
	combined := password + salt
	h := md5.Sum([]byte(combined))
	return hex.EncodeToString(h[:])
}

// generateSalt produces a random alphanumeric salt.
func generateSalt(n int) string {
	bytes := make([]byte, n)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)[:n]
}
