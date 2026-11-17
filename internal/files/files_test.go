package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func setup(t *testing.T) (*Manager, string, string) {
	t.Helper()
	base := t.TempDir()
	music := filepath.Join(base, "music")
	outside := filepath.Join(base, "secret")
	for _, d := range []string{filepath.Join(music, "Artist", "Album (2020)"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(music, "Artist", "Album (2020)", "01 - Song.flac"), []byte("abc"), 0o644)
	_ = os.WriteFile(filepath.Join(music, "Artist", "Album (2020)", "cover.jpg"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o644)
	return NewManager(Root{Name: "music", Label: "Music", Path: music}), music, outside
}

func TestResolveBlocksTraversal(t *testing.T) {
	m, music, _ := setup(t)
	for _, p := range []string{"../secret", "Artist/../../secret", "/../secret"} {
		abs, err := m.Resolve("music", p)
		if err != nil {
			continue
		}
		// "/../secret" style input is anchored to the root instead of escaping it.
		if rel, _ := filepath.Rel(music, abs); rel == ".." || filepath.IsAbs(rel) || len(rel) >= 2 && rel[:2] == ".." {
			t.Fatalf("%q resolved outside root: %s", p, abs)
		}
	}
	if _, err := m.Resolve("nope", ""); err == nil {
		t.Fatal("unknown root should fail")
	}
}

func TestResolveBlocksSymlinkEscape(t *testing.T) {
	m, music, outside := setup(t)
	if err := os.Symlink(outside, filepath.Join(music, "link")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if _, err := m.Resolve("music", "link/keep.txt"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("expected ErrOutsideRoot through symlink, got %v", err)
	}
	if err := m.Delete("music", "link/keep.txt"); err == nil {
		t.Fatal("delete through symlink must fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("file outside root was touched")
	}
}

func TestListRenameMoveDelete(t *testing.T) {
	m, music, _ := setup(t)

	l, err := m.List("music", "Artist/Album (2020)")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 2 || l.AudioCount != 1 || l.TotalSize != 4 {
		t.Fatalf("unexpected listing: %+v", l)
	}

	if _, err := m.Rename("music", "Artist/Album (2020)/01 - Song.flac", "../evil.flac"); err == nil {
		t.Fatal("rename with slash must fail")
	}
	np, err := m.Rename("music", "Artist/Album (2020)/01 - Song.flac", "01 - Renamed.flac")
	if err != nil || np != "Artist/Album (2020)/01 - Renamed.flac" {
		t.Fatalf("rename: %v %q", err, np)
	}

	np, err = m.Move("music", "Artist/Album (2020)", "Other Artist")
	if err != nil || np != "Other Artist/Album (2020)" {
		t.Fatalf("move: %v %q", err, np)
	}
	if _, err := m.Move("music", "Other Artist", "Other Artist/Album (2020)"); err == nil {
		t.Fatal("moving a folder into itself must fail")
	}

	if _, err := m.Mkdir("music", "", "New"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("music", ""); err == nil {
		t.Fatal("deleting the root must fail")
	}
	if err := m.Delete("music", "Other Artist/Album (2020)"); err != nil {
		t.Fatal(err)
	}
	m.RemoveEmptyParents("music", filepath.Join(music, "Other Artist", "Album (2020)"))
	if _, err := os.Stat(filepath.Join(music, "Other Artist")); !os.IsNotExist(err) {
		t.Fatal("empty parent should be removed")
	}
	if _, err := os.Stat(music); err != nil {
		t.Fatal("root must survive RemoveEmptyParents")
	}
}
