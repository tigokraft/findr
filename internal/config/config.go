package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents findr's complete application configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Library    LibraryConfig    `yaml:"library"`
	Indexer    IndexerConfig    `yaml:"indexer"`
	Downloader DownloaderConfig `yaml:"downloader"`
	Verifier   VerifierConfig   `yaml:"verifier"`
	Navidrome  NavidromeConfig  `yaml:"navidrome"`
	Database   DatabaseConfig   `yaml:"database"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type LibraryConfig struct {
	MusicPath    string `yaml:"music_path"`
	DownloadPath string `yaml:"download_path"`
	FolderFormat string `yaml:"folder_format"` // e.g. "{Artist}/{Album} ({Year})"
	FileFormat   string `yaml:"file_format"`   // e.g. "{Track:02d} - {Title}"
}

type IndexerConfig struct {
	DefaultProvider string        `yaml:"default_provider"` // "deezer", "qobuz", "spotify"
	Spotify         SpotifyConfig `yaml:"spotify"`
	Qobuz           QobuzConfig   `yaml:"qobuz"`
}

type SpotifyConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

type QobuzConfig struct {
	AppID     string `yaml:"app_id"`
	AppSecret string `yaml:"app_secret"`
}

type DownloaderConfig struct {
	Preferred string       `yaml:"preferred"` // "qobuz", "slskd"
	Qobuz     QobuzDLConfig `yaml:"qobuz"`
	Slskd     SlskdConfig   `yaml:"slskd"`
}

type QobuzDLConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Username      string `yaml:"username"`
	Password      string `yaml:"password"`
	AppID         string `yaml:"app_id"`
	AppSecret     string `yaml:"app_secret"`
	TargetQuality string `yaml:"target_quality"` // "flac_24_96", "flac_16_44", "mp3_320"
}

type SlskdConfig struct {
	Enabled     bool   `yaml:"enabled"`
	URL         string `yaml:"url"`
	APIKey      string `yaml:"api_key"`
	DownloadDir string `yaml:"download_dir"`
}

type VerifierConfig struct {
	DurationToleranceSec int     `yaml:"duration_tolerance_sec"` // default: 4
	MinConfidenceScore   float64 `yaml:"min_confidence_score"`   // default: 0.85
	AllowPartialMatch    bool    `yaml:"allow_partial_match"`    // default: false
}

type NavidromeConfig struct {
	Enabled   bool   `yaml:"enabled"`
	URL       string `yaml:"url"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	TokenAuth bool   `yaml:"token_auth"` // Use MD5 salt+token authentication
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// DefaultConfig provides sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8686,
		},
		Library: LibraryConfig{
			MusicPath:    "./music",
			DownloadPath: "./downloads",
			FolderFormat: "{Artist}/{Album} ({Year})",
			FileFormat:   "{Track:02d} - {Title}",
		},
		Indexer: IndexerConfig{
			DefaultProvider: "deezer",
			Qobuz: QobuzConfig{
				AppID: "305874112", // Standard Qobuz public client ID
			},
		},
		Downloader: DownloaderConfig{
			Preferred: "qobuz",
			Qobuz: QobuzDLConfig{
				Enabled:       true,
				AppID:         "305874112",
				TargetQuality: "flac_16_44",
			},
			Slskd: SlskdConfig{
				Enabled: false,
				URL:     "http://localhost:5030",
			},
		},
		Verifier: VerifierConfig{
			DurationToleranceSec: 4,
			MinConfidenceScore:   0.85,
			AllowPartialMatch:    false,
		},
		Navidrome: NavidromeConfig{
			Enabled:   false,
			URL:       "http://localhost:4533",
			TokenAuth: true,
		},
		Database: DatabaseConfig{
			Path: "findr.db",
		},
	}
}

// Load reads config from path, falling back to defaults for unspecified values.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // Return default if not exists
		}
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config yaml: %w", err)
	}

	// Environment variable overrides
	if val := os.Getenv("FINDR_MUSIC_PATH"); val != "" {
		cfg.Library.MusicPath = val
	}
	if val := os.Getenv("FINDR_DOWNLOAD_PATH"); val != "" {
		cfg.Library.DownloadPath = val
	}
	if val := os.Getenv("FINDR_QOBUZ_USERNAME"); val != "" {
		cfg.Downloader.Qobuz.Username = val
	}
	if val := os.Getenv("FINDR_QOBUZ_PASSWORD"); val != "" {
		cfg.Downloader.Qobuz.Password = val
	}
	if val := os.Getenv("FINDR_NAVIDROME_URL"); val != "" {
		cfg.Navidrome.URL = val
	}
	if val := os.Getenv("FINDR_NAVIDROME_USER"); val != "" {
		cfg.Navidrome.Username = val
	}
	if val := os.Getenv("FINDR_NAVIDROME_PASSWORD"); val != "" {
		cfg.Navidrome.Password = val
	}

	return cfg, nil
}
