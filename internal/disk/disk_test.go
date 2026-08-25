package disk_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SphericalKat/telemirror/internal/disk"
)

func TestUsageReportsFilesystemSpace(t *testing.T) {
	space, err := disk.Usage(t.TempDir())
	if err != nil {
		t.Fatalf("disk.Usage() error = %v", err)
	}
	if space.TotalBytes <= 0 {
		t.Errorf("total bytes = %d, want a positive size", space.TotalBytes)
	}
	if space.UsedBytes < 0 {
		t.Errorf("used bytes = %d, want zero or more", space.UsedBytes)
	}
	if space.FreeBytes < 0 {
		t.Errorf("free bytes = %d, want zero or more", space.FreeBytes)
	}
	if space.UsedBytes+space.FreeBytes > space.TotalBytes {
		t.Errorf("used %d plus free %d exceeds the total %d", space.UsedBytes, space.FreeBytes, space.TotalBytes)
	}
}

func TestUsageReportsTheSpaceOfTheFilesystemThatHoldsThePath(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested", "deeper")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", nested, err)
	}

	rootSpace, err := disk.Usage(root)
	if err != nil {
		t.Fatalf("disk.Usage(%s) error = %v", root, err)
	}
	nestedSpace, err := disk.Usage(nested)
	if err != nil {
		t.Fatalf("disk.Usage(%s) error = %v", nested, err)
	}
	// The total size names the file system; the used and free space of a
	// live volume change between two reads.
	if nestedSpace.TotalBytes != rootSpace.TotalBytes {
		t.Errorf("total space of %s = %d, want the total space of %s = %d",
			nested, nestedSpace.TotalBytes, root, rootSpace.TotalBytes)
	}
}

func TestUsageReportsAClearErrorForAMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	_, err := disk.Usage(missing)
	if err == nil {
		t.Fatal("disk.Usage() on a missing path returned no error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the path %s", err, missing)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to report a missing path", err)
	}
}
