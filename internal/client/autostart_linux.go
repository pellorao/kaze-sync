package client

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func unitText(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=kaze-sync client

[Service]
ExecStart="%s" run
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, exe)
}

// ConfigureAutostart installs and (re)starts a systemd user unit running "exe run".
func ConfigureAutostart(exe string) error {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	return configureAutostart(exe, cfg, func(args ...string) error {
		out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user %v: %w: %s", args, err, out)
		}
		return nil
	})
}

func configureAutostart(exe, configDir string, systemctl func(args ...string) error) error {
	dir := filepath.Join(configDir, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "kaze-sync.service"), []byte(unitText(exe)), 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "kaze-sync.service"); err != nil {
		return err
	}
	// restart, not "enable --now": an agent that is already running would go on with the old device.json.
	return systemctl("restart", "kaze-sync.service")
}

// AutostartDir returns the directory of the program the user unit starts, or "" when there is no unit.
func AutostartDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(cfg, "systemd", "user", "kaze-sync.service"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return unitExecDir(string(b)), nil
}
