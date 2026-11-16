package system

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DiskInfo represents filesystem capacity and utilization for a given directory or mount.
type DiskInfo struct {
	Path        string  `json:"path"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	PercentUsed float64 `json:"percent_used"`
	FreeHuman   string  `json:"free_human"`
	TotalHuman  string  `json:"total_human"`
	UsedHuman   string  `json:"used_human"`
}

// GetDiskUsage calculates storage metrics for a path.
func GetDiskUsage(path string) (*DiskInfo, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	// Find the nearest existing ancestor path if path does not exist yet
	checkPath := absPath
	for {
		if _, err := os.Stat(checkPath); err == nil {
			break
		}
		parent := filepath.Dir(checkPath)
		if parent == checkPath {
			break
		}
		checkPath = parent
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(checkPath, &stat); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", checkPath, err)
	}

	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	free := uint64(stat.Bavail) * uint64(stat.Bsize)
	used := uint64(0)
	if total > free {
		used = total - free
	}

	percent := 0.0
	if total > 0 {
		percent = (float64(used) / float64(total)) * 100.0
	}

	return &DiskInfo{
		Path:        absPath,
		FreeBytes:   free,
		TotalBytes:  total,
		UsedBytes:   used,
		PercentUsed: percent,
		FreeHuman:   FormatBytes(free),
		TotalHuman:  FormatBytes(total),
		UsedHuman:   FormatBytes(used),
	}, nil
}

// GetDisksUsage returns disk metrics for multiple unique paths (e.g. music, downloads, root).
func GetDisksUsage(paths []string) []DiskInfo {
	seen := make(map[string]bool)
	var disks []DiskInfo

	for _, p := range paths {
		if p == "" {
			continue
		}
		info, err := GetDiskUsage(p)
		if err != nil {
			continue
		}
		if seen[info.Path] {
			continue
		}
		seen[info.Path] = true
		disks = append(disks, *info)
	}

	// Always ensure root "/" is checked if not already present
	rootPath, _ := filepath.Abs("/")
	if !seen[rootPath] {
		if info, err := GetDiskUsage("/"); err == nil {
			disks = append(disks, *info)
		}
	}

	return disks
}

// FormatBytes formats byte count into human readable units (e.g. 54.2 GB).
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
