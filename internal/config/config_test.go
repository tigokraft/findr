package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoadAndSave(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")

	// 1. Loading non-existent file should return defaults
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("expected nil error on missing config, got %v", err)
	}
	if cfg.Server.Port != 8686 {
		t.Errorf("expected port 8686, got %d", cfg.Server.Port)
	}
	if !cfg.Import.DeleteAfterImport {
		t.Errorf("expected DeleteAfterImport to default to true")
	}

	// 2. Modify and Save
	cfg.Server.Port = 9090
	cfg.Library.MusicPath = "/media/test_music"
	cfg.Navidrome.Enabled = true
	cfg.Navidrome.URL = "http://navidrome:4533"
	cfg.Downloader.Qobuz.UserToken = "secret-token-123"

	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("failed to save config: %v", err)
	}

	// 3. Reload and verify
	reloaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to reload config: %v", err)
	}

	if reloaded.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", reloaded.Server.Port)
	}
	if reloaded.Library.MusicPath != "/media/test_music" {
		t.Errorf("expected /media/test_music, got %s", reloaded.Library.MusicPath)
	}
	if !reloaded.Navidrome.Enabled {
		t.Errorf("expected navidrome enabled")
	}
	if reloaded.Downloader.Qobuz.UserToken != "secret-token-123" {
		t.Errorf("expected token preserved, got %s", reloaded.Downloader.Qobuz.UserToken)
	}

	// Verify file exists on disk
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config file was not created: %v", err)
	}
}
