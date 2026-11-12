//go:build windows

package main

import "os/exec"

// Windows has no process groups in this sense; Linux-side agents are never started here.
func setProcessGroup(c *exec.Cmd) {}

func signalGroup(c *exec.Cmd, kill bool) { c.Process.Kill() }
