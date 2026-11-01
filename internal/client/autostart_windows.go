package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// ConfigureAutostart registers the windowless agent in the HKCU Run key and starts it now.
func ConfigureAutostart(exe string) error {
	agent := filepath.Join(filepath.Dir(exe), "kaze-syncc-agent.exe")
	if _, err := os.Stat(agent); err != nil {
		agent = exe
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("kaze-sync", `"`+agent+`" run`); err != nil {
		return err
	}
	cmd := exec.Command(agent, "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	return cmd.Start()
}
