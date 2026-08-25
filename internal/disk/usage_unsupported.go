//go:build !linux && !darwin

package disk

// Usage reports ErrUnsupported, because the platform provides no
// statfs-compatible system call.
func Usage(path string) (Space, error) {
	return Space{}, ErrUnsupported
}
