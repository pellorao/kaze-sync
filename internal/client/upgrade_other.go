//go:build !windows

package client

import "errors"

// handoffInstaller is only used on Windows, where a program cannot replace itself; elsewhere upgrade installs
// from the downloaded file directly.
func handoffInstaller(string, bool) error { return errors.New("not supported on this system") }
