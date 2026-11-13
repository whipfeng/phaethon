//go:build !windows

package admin

import "syscall"

// freeDiskBytes returns the bytes available to an unprivileged user on the
// filesystem containing path.
func freeDiskBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
