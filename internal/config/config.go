package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config represents findr's complete application configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server" json:"server"`
	Library    LibraryConfig    `yaml:"library" json:"library"`
	Import     ImportConfig     `yaml:"import" json:"import"`
	Export     ExportConfig     `yaml:"export" json:"export"`
	Indexer    IndexerConfig    `yaml:"indexer" json:"indexer"`
	Downloader DownloaderConfig `yaml:"downloader" json:"downloader"`
	Verifier   VerifierConfig   `yaml:"verifier" json:"verifier"`
	Navidrome  NavidromeConfig  `yaml:"navidrome" json:"navidrome"`
	Database   DatabaseConfig   `yaml:"database" json:"database"`
}

type ServerConfig struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
}

type LibraryConfig struct {
	MusicPath    string `yaml:"music_path" json:"music_path"`
	DownloadPath string `yaml:"download_path" json:"download_path"`
	FolderFormat string `yaml:"folder_format" json:"folder_format"` // e.g. "{Artist}/{Album} ({Year})"
	FileFormat   string `yaml:"file_format" json:"file_format"`     // e.g. "{Track:02d} - {Title}"
}

type ImportConfig struct {
	DeleteAfterImport bool   `yaml:"delete_after_import" json:"delete_after_import"` // Delete downloaded staging directory after import
	MoveOrCopy        string `yaml:"move_or_copy" json:"move_or_copy"`               // "move" or "copy"
	CleanEmptyFolders bool   `yaml:"clean_empty_folders" json:"clean_empty_folders"` // Delete empty directories
}

type ExportConfig struct {
	DownloadCoverArt bool   `yaml:"download_cover_art" json:"download_cover_art"` // Fetch cover.jpg
	CoverFilename    string `yaml:"cover_filename" json:"cover_filename"`         // e.g. "cover.jpg"
	WriteNFO         bool   `yaml:"write_nfo" json:"write_nfo"`                   // Write album.nfo for Kodi/Plex
	GenerateM3U      bool   `yaml:"generate_m3u" json:"generate_m3u"`             // Create playlist.m3u
}

type IndexerConfig struct {
	DefaultProvider string        `yaml:"default_provider" json:"default_provider"` // "deezer", "qobuz", "spotify"
	Spotify         SpotifyConfig `yaml:"spotify" json:"spotify"`
	Qobuz           QobuzConfig   `yaml:"qobuz" json:"qobuz"`
}

type SpotifyConfig struct {
	ClientID     string `yaml:"client_id" json:"client_id"`
	ClientSecret string `yaml:"client_secret" json:"client_secret"`
}

type QobuzConfig struct {
	AppID     string `yaml:"app_id" json:"app_id"`
	AppSecret string `yaml:"app_secret" json:"app_secret"`
	UserToken string `yaml:"user_token" json:"user_token"`
}

type DownloaderConfig struct {
	Preferred              string        `yaml:"preferred" json:"preferred"` // "qobuz", "slskd"
	MaxConcurrentDownloads int           `yaml:"max_concurrent_downloads" json:"max_concurrent_downloads"` // Concurrency limit for downloads
	Qobuz                  QobuzDLConfig `yaml:"qobuz" json:"qobuz"`
	Slskd                  SlskdConfig   `yaml:"slskd" json:"slskd"`
}

type QobuzDLConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Username      string `yaml:"username" json:"username"`
	Password      string `yaml:"password" json:"password"`
	UserToken     string `yaml:"user_token" json:"user_token"`
	AppID         string `yaml:"app_id" json:"app_id"`
	AppSecret     string `yaml:"app_secret" json:"app_secret"`
	TargetQuality string `yaml:"target_quality" json:"target_quality"` // "flac_24_192", "flac_24_96", "flac_16_44", "mp3_320"
}

type SlskdConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	URL         string `yaml:"url" json:"url"`
	APIKey      string `yaml:"api_key" json:"api_key"`
	DownloadDir string `yaml:"download_dir" json:"download_dir"`
}

type VerifierConfig struct {
	DurationToleranceSec int     `yaml:"duration_tolerance_sec" json:"duration_tolerance_sec"` // default: 4
	MinConfidenceScore   float64 `yaml:"min_confidence_score" json:"min_confidence_score"`     // default: 0.85
	AllowPartialMatch    bool    `yaml:"allow_partial_match" json:"allow_partial_match"`         // default: false
}

type NavidromeConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	URL       string `yaml:"url" json:"url"`
	Username  string `yaml:"username" json:"username"`
	Password  string `yaml:"password" json:"password"`
	TokenAuth bool   `yaml:"token_auth" json:"token_auth"` // Use MD5 salt+token authentication
}

type DatabaseConfig struct {
	Path string `yaml:"path" json:"path"`
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
		Import: ImportConfig{
			DeleteAfterImport: true,
			MoveOrCopy:        "move",
			CleanEmptyFolders: true,
		},
		Export: ExportConfig{
			DownloadCoverArt: true,
			CoverFilename:    "cover.jpg",
			WriteNFO:         false,
			GenerateM3U:      false,
		},
		Indexer: IndexerConfig{
			DefaultProvider: "deezer",
			Qobuz: QobuzConfig{
				AppID:     "798273057", // Standard Qobuz public client ID
				AppSecret: "abb21364945c0583309667d13ca3d93a",
			},
		},
		Downloader: DownloaderConfig{
			Preferred:              "qobuz",
			MaxConcurrentDownloads: 2,
			Qobuz: QobuzDLConfig{
				Enabled:       true,
				AppID:         "798273057",
				AppSecret:     "abb21364945c0583309667d13ca3d93a",
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
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading config file %s: %w", path, err)
		}
	} else {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config yaml: %w", err)
		}
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
	if val := os.Getenv("FINDR_QOBUZ_USER_TOKEN"); val != "" {
		cfg.Downloader.Qobuz.UserToken = val
		cfg.Indexer.Qobuz.UserToken = val
	}
	if val := os.Getenv("FINDR_QOBUZ_APP_ID"); val != "" {
		cfg.Downloader.Qobuz.AppID = val
		cfg.Indexer.Qobuz.AppID = val
	}
	if val := os.Getenv("FINDR_QOBUZ_APP_SECRET"); val != "" {
		cfg.Downloader.Qobuz.AppSecret = val
		cfg.Indexer.Qobuz.AppSecret = val
	}
	if val := os.Getenv("FINDR_DOWNLOADER_PREFERRED"); val != "" {
		cfg.Downloader.Preferred = val
	}
	if val := os.Getenv("FINDR_SLSKD_ENABLED"); val != "" {
		cfg.Downloader.Slskd.Enabled = val == "true" || val == "1"
	}
	if val := os.Getenv("FINDR_SLSKD_URL"); val != "" {
		cfg.Downloader.Slskd.URL = val
	}
	if val := os.Getenv("FINDR_SLSKD_API_KEY"); val != "" {
		cfg.Downloader.Slskd.APIKey = val
	}
	if val := os.Getenv("FINDR_SLSKD_DOWNLOAD_DIR"); val != "" {
		cfg.Downloader.Slskd.DownloadDir = val
	}
	if cfg.Downloader.Slskd.DownloadDir == "" {
		cfg.Downloader.Slskd.DownloadDir = cfg.Library.DownloadPath
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
	if val := os.Getenv("FINDR_MAX_CONCURRENT_DOWNLOADS"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			cfg.Downloader.MaxConcurrentDownloads = n
		}
	}
	if cfg.Downloader.MaxConcurrentDownloads <= 0 {
		cfg.Downloader.MaxConcurrentDownloads = 2
	}

	// Auto-fill well-known app secrets if missing
	if cfg.Downloader.Qobuz.AppSecret == "" {
		if cfg.Downloader.Qobuz.AppID == "798273057" {
			cfg.Downloader.Qobuz.AppSecret = "abb21364945c0583309667d13ca3d93a"
		} else if cfg.Downloader.Qobuz.AppID == "305874112" {
			cfg.Downloader.Qobuz.AppSecret = "d9f2e3798aa12ff5e13589b14c33f81e"
		}
	}
	if cfg.Indexer.Qobuz.AppSecret == "" {
		if cfg.Indexer.Qobuz.AppID == "798273057" {
			cfg.Indexer.Qobuz.AppSecret = "abb21364945c0583309667d13ca3d93a"
		} else if cfg.Indexer.Qobuz.AppID == "305874112" {
			cfg.Indexer.Qobuz.AppSecret = "d9f2e3798aa12ff5e13589b14c33f81e"
		}
	}

	return cfg, nil
}

// Save marshals cfg to YAML and writes safely to path.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config yaml: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("creating config dir: %w", err)
		}
	}

	tmpFile := path + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("writing temporary config file: %w", err)
	}

	if err := os.Rename(tmpFile, path); err != nil {
		return fmt.Errorf("saving config file %s: %w", path, err)
	}

	return nil
}
