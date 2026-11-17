// Package files provides sandboxed file management over findr's library and
// staging folders. Every path a caller passes is relative to a named root and is
// rejected if it resolves (including through symlinks) outside that root.
package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrOutsideRoot is returned for paths that escape their root.
var ErrOutsideRoot = errors.New("path is outside the allowed folder")

// AudioExtensions are the file types findr treats as audio.
var AudioExtensions = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".aac": true, ".wav": true,
	".ogg": true, ".opus": true, ".aiff": true, ".aif": true, ".alac": true, ".wv": true, ".ape": true,
}

// Root is a named top-level folder the manager may operate in.
type Root struct {
	Name  string `json:"name"`  // "music", "downloads"
	Label string `json:"label"` // human label
	Path  string `json:"path"`  // absolute path on disk
}

// Entry describes one file or folder.
type Entry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"` // relative to the root, slash-separated
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Ext      string    `json:"ext,omitempty"`
	IsAudio  bool      `json:"is_audio"`
	IsImage  bool      `json:"is_image"`
	Children int       `json:"children,omitempty"` // direct entries, for folders
}

// Listing is the content of one folder.
type Listing struct {
	Root       string  `json:"root"`
	Path       string  `json:"path"`
	AbsPath    string  `json:"abs_path"`
	Entries    []Entry `json:"entries"`
	TotalSize  int64   `json:"total_size"`  // sum of direct file sizes
	AudioCount int     `json:"audio_count"` // direct audio files
}

// Manager performs file operations within a fixed set of roots.
type Manager struct {
	roots map[string]Root
	order []string
}

// NewManager builds a manager. Roots with empty paths are skipped.
func NewManager(roots ...Root) *Manager {
	m := &Manager{roots: make(map[string]Root)}
	for _, r := range roots {
		if strings.TrimSpace(r.Path) == "" {
			continue
		}
		abs, err := filepath.Abs(r.Path)
		if err != nil {
			continue
		}
		r.Path = filepath.Clean(abs)
		if _, dup := m.roots[r.Name]; !dup {
			m.order = append(m.order, r.Name)
		}
		m.roots[r.Name] = r
	}
	return m
}

// Roots lists configured roots in registration order.
func (m *Manager) Roots() []Root {
	out := make([]Root, 0, len(m.order))
	for _, n := range m.order {
		out = append(out, m.roots[n])
	}
	return out
}

// Resolve maps root + relative path to an absolute path, enforcing containment.
func (m *Manager) Resolve(rootName, rel string) (string, error) {
	root, ok := m.roots[rootName]
	if !ok {
		return "", fmt.Errorf("unknown folder %q", rootName)
	}
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	cleanRel := filepath.Clean("/" + filepath.FromSlash(rel)) // "/" anchors so ".." cannot climb
	abs := filepath.Join(root.Path, cleanRel)
	if !within(root.Path, abs) {
		return "", ErrOutsideRoot
	}

	// Follow symlinks on the deepest existing ancestor so a link inside the
	// library cannot point the operation somewhere else.
	realRoot, err := filepath.EvalSymlinks(root.Path)
	if err != nil {
		return "", fmt.Errorf("folder %s is not available: %w", root.Path, err)
	}
	probe := abs
	for {
		if real, err := filepath.EvalSymlinks(probe); err == nil {
			if !within(realRoot, real) {
				return "", ErrOutsideRoot
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return abs, nil
}

// RelPath converts an absolute path back to a root-relative slash path.
func (m *Manager) RelPath(rootName, abs string) (string, bool) {
	root, ok := m.roots[rootName]
	if !ok {
		return "", false
	}
	abs = filepath.Clean(abs)
	if !within(root.Path, abs) {
		return "", false
	}
	rel, err := filepath.Rel(root.Path, abs)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func isRoot(m *Manager, rootName, abs string) bool {
	return filepath.Clean(abs) == m.roots[rootName].Path
}

// List returns the direct contents of a folder, folders first.
func (m *Manager) List(rootName, rel string) (*Listing, error) {
	abs, err := m.Resolve(rootName, rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", rel)
	}
	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	relClean, _ := m.RelPath(rootName, abs)
	listing := &Listing{Root: rootName, Path: relClean, AbsPath: abs, Entries: make([]Entry, 0, len(dirEntries))}
	for _, de := range dirEntries {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		childAbs := filepath.Join(abs, name)
		childRel, _ := m.RelPath(rootName, childAbs)
		e := Entry{Name: name, Path: childRel, IsDir: fi.IsDir(), ModTime: fi.ModTime()}
		if fi.IsDir() {
			if sub, err := os.ReadDir(childAbs); err == nil {
				for _, s := range sub {
					if !strings.HasPrefix(s.Name(), ".") {
						e.Children++
					}
				}
			}
		} else {
			e.Size = fi.Size()
			e.Ext = strings.ToLower(filepath.Ext(name))
			e.IsAudio = AudioExtensions[e.Ext]
			e.IsImage = e.Ext == ".jpg" || e.Ext == ".jpeg" || e.Ext == ".png" || e.Ext == ".webp"
			listing.TotalSize += e.Size
			if e.IsAudio {
				listing.AudioCount++
			}
		}
		listing.Entries = append(listing.Entries, e)
	}
	sort.SliceStable(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return listing, nil
}

// DirSize walks a folder and returns total bytes and file count.
func DirSize(abs string) (int64, int) {
	var size int64
	var count int
	_ = filepath.WalkDir(abs, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			size += fi.Size()
			count++
		}
		return nil
	})
	return size, count
}

// Delete removes a file or folder (recursively). The root itself cannot be deleted.
func (m *Manager) Delete(rootName, rel string) error {
	abs, err := m.Resolve(rootName, rel)
	if err != nil {
		return err
	}
	if isRoot(m, rootName, abs) {
		return fmt.Errorf("refusing to delete the %s folder itself", rootName)
	}
	if _, err := os.Lstat(abs); err != nil {
		return err
	}
	return os.RemoveAll(abs)
}

// validName rejects names that would change directory or are hidden/special.
func validName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("name cannot be empty")
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("name cannot contain slashes")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("name cannot start with a dot")
	}
	return nil
}

// Rename renames an entry within its current folder. Returns the new relative path.
func (m *Manager) Rename(rootName, rel, newName string) (string, error) {
	if err := validName(newName); err != nil {
		return "", err
	}
	abs, err := m.Resolve(rootName, rel)
	if err != nil {
		return "", err
	}
	if isRoot(m, rootName, abs) {
		return "", fmt.Errorf("cannot rename the %s folder itself", rootName)
	}
	dest := filepath.Join(filepath.Dir(abs), strings.TrimSpace(newName))
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("%s already exists", newName)
	}
	if err := os.Rename(abs, dest); err != nil {
		return "", err
	}
	out, _ := m.RelPath(rootName, dest)
	return out, nil
}

// Move moves an entry into another folder of the same root, creating it if needed.
// Returns the new relative path.
func (m *Manager) Move(rootName, rel, destDirRel string) (string, error) {
	abs, err := m.Resolve(rootName, rel)
	if err != nil {
		return "", err
	}
	if isRoot(m, rootName, abs) {
		return "", fmt.Errorf("cannot move the %s folder itself", rootName)
	}
	destDir, err := m.Resolve(rootName, destDirRel)
	if err != nil {
		return "", err
	}
	if destDir == abs || within(abs, destDir) {
		return "", fmt.Errorf("cannot move a folder into itself")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, filepath.Base(abs))
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("%s already exists in the destination", filepath.Base(abs))
	}
	if err := os.Rename(abs, dest); err != nil {
		return "", err
	}
	out, _ := m.RelPath(rootName, dest)
	return out, nil
}

// Mkdir creates a folder inside parentRel. Returns its relative path.
func (m *Manager) Mkdir(rootName, parentRel, name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	parent, err := m.Resolve(rootName, parentRel)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(parent, strings.TrimSpace(name))
	if err := os.Mkdir(dest, 0o755); err != nil {
		return "", err
	}
	out, _ := m.RelPath(rootName, dest)
	return out, nil
}

// RemoveEmptyParents deletes empty folders from abs's parent upward, stopping at the root.
func (m *Manager) RemoveEmptyParents(rootName, abs string) {
	root, ok := m.roots[rootName]
	if !ok {
		return
	}
	dir := filepath.Dir(abs)
	for within(root.Path, dir) && dir != root.Path {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
