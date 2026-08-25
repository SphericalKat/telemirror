//go:build linux || darwin

package disk

import (
	"fmt"
	"syscall"
)

// Usage reports the space of the file system that holds path. It reads
// one file-system status structure, like the df command does: the total
// space counts every data block, the used space counts the blocks that
// are not free, and the free space counts the blocks that an
// unprivileged process can allocate.
func Usage(path string) (Space, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Space{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	block := uint64(st.Bsize)
	return Space{
		TotalBytes: int64(st.Blocks * block),
		UsedBytes:  int64((st.Blocks - st.Bfree) * block),
		FreeBytes:  int64(st.Bavail * block),
	}, nil
}
