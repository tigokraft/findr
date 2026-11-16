package system

import (
	"testing"
)

func TestGetDiskUsage(t *testing.T) {
	info, err := GetDiskUsage(".")
	if err != nil {
		t.Fatalf("failed to get disk usage for current dir: %v", err)
	}

	if info.TotalBytes == 0 {
		t.Errorf("expected total bytes > 0")
	}
	if info.TotalHuman == "" {
		t.Errorf("expected formatted total human string")
	}
	if info.PercentUsed < 0 || info.PercentUsed > 100 {
		t.Errorf("unexpected percent used: %f", info.PercentUsed)
	}
}

func TestGetDisksUsage(t *testing.T) {
	disks := GetDisksUsage([]string{".", "/nonexistent/test/path"})
	if len(disks) == 0 {
		t.Fatalf("expected at least one disk returned")
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		bytes    uint64
		expected string
	}{
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
	}

	for _, c := range cases {
		got := FormatBytes(c.bytes)
		if got != c.expected {
			t.Errorf("FormatBytes(%d) = %q, expected %q", c.bytes, got, c.expected)
		}
	}
}
