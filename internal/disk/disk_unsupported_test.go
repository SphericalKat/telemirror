//go:build !linux && !darwin

package disk_test

import (
	"errors"
	"testing"

	"github.com/SphericalKat/telemirror/internal/disk"
)

func TestUsageReportsUnsupportedPlatform(t *testing.T) {
	_, err := disk.Usage(t.TempDir())
	if !errors.Is(err, disk.ErrUnsupported) {
		t.Errorf("disk.Usage() error = %v, want disk.ErrUnsupported", err)
	}
}
