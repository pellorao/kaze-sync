//go:build !windows

package client

import (
	"syscall"
	"time"
)

func cpuTime() (user, sys time.Duration) {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano()), time.Duration(ru.Stime.Nano())
}
