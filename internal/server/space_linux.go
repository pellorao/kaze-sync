package server

import "syscall"

// freeBytes is the space available to unprivileged writers on the file system of dir; ok is false when unknown.
func freeBytes(dir string) (n uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
