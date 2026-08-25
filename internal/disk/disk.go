// Package disk reports file-system space through Go system calls.
//
// Usage returns the total, used, and available space of the file system
// that holds a path, so /disk builds no shell command from configuration
// input. Linux and macOS support disk-space reporting.
package disk

import "errors"

// ErrUnsupported reports that the platform does not support disk-space
// reporting. Linux and macOS support it.
var ErrUnsupported = errors.New("disk: platform does not support disk-space reporting")

// Space is the space of one file system, in bytes.
type Space struct {
	// TotalBytes is the total space of the file system.
	TotalBytes int64

	// UsedBytes is the space that the file system holds in use.
	UsedBytes int64

	// FreeBytes is the space available to an unprivileged process.
	FreeBytes int64
}
