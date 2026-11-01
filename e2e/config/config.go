// Package config is the e2e configuration: a JSON file outside the repository
// (docs/plan.md, M3) read by both the VM builder and the driver.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Vmrun          string `json:"vmrun"`          // path of vmrun.exe as seen from WSL; vmware-vdiskmanager.exe is expected next to it
	Vmx            string `json:"vmx"`            // Windows path of the VM's .vmx
	Snapshot       string `json:"snapshot"`       // clean baseline snapshot
	IP             string `json:"ip"`             // fixed address of the VM on VMware's NAT network, outside its DHCP pool (the gateway is x.x.x.2)
	SSH            SSH    `json:"ssh"`            //
	WindowsWorkDir string `json:"windowsWorkDir"` // Windows path for the Windows client's files
	Tools          string `json:"tools"`          // dpkg -x root with qemu-img, make and zstd; builder only
	Cache          string `json:"cache"`          // download cache; builder only
}

type SSH struct {
	User         string `json:"user"`
	Port         int    `json:"port"`
	IdentityFile string `json:"identityFile"`
	HostKey      string `json:"hostKey"` // written by the builder
}

// Load reads the file and rejects missing required keys.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Vmrun == "" || c.Vmx == "" || c.Snapshot == "" || c.IP == "" || c.SSH.IdentityFile == "" {
		return nil, fmt.Errorf("%s: vmrun, vmx, snapshot, ip and ssh.identityFile are required", path)
	}
	if c.SSH.User == "" {
		c.SSH.User = "root"
	}
	if c.SSH.Port == 0 {
		c.SSH.Port = 22
	}
	return &c, nil
}

// Save writes the file back (the builder stores the host key).
func (c *Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
