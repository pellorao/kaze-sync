package client

import (
	"os/exec"
	"syscall"
)

const createNewConsole = 0x00000010

// handoffInstaller starts the downloaded setup program in a console window of its own and lets go of it, so that
// this process can end and free kaze-syncc.exe for the replacement.
func handoffInstaller(file string, force bool) error {
	var args []string
	if force {
		args = append(args, "--force")
	}
	cmd := exec.Command(file, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
