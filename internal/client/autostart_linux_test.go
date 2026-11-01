package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUserUnit(t *testing.T) {
	cfg := t.TempDir()
	var cmds []string
	err := configureAutostart("/opt/kaze sync/kaze-syncc", cfg, func(args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(filepath.Join(cfg, "systemd/user/kaze-sync.service"))
	if !strings.Contains(string(unit), `ExecStart="/opt/kaze sync/kaze-syncc" run`) || !strings.Contains(string(unit), "WantedBy=default.target") {
		t.Fatalf("unit:\n%s", unit)
	}
	if len(cmds) != 2 || cmds[0] != "daemon-reload" || cmds[1] != "enable --now kaze-sync.service" {
		t.Fatalf("commands %v", cmds)
	}
}
