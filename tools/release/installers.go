package main

import (
	"crypto/sha256"
	"debug/pe"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/version"
)

//go:embed installer.sh
var shellHeader string

// Names of the client installers inside the server installer's payload; the server publishes them under these
// names (internal/server clientPackageFilename).
const (
	clientLinuxPackage   = "client-linux-amd64.sh"
	clientWindowsPackage = "client-windows-amd64-setup.exe"
)

// PE subsystems of the Windows programs: the agent must not open a console window at every login.
const (
	subsystemGUI     = 2
	subsystemConsole = 3
)

// binaries are the built programs the installers carry.
type binaries struct {
	Syncd, Syncc, WinSyncc, WinAgent, WinSetup []byte
}

// installers are the three installer files of a release, by file name.
type installers map[string][]byte

var placeholder = regexp.MustCompile(`@[A-Z_]+@`)

var agentVersionLine = regexp.MustCompile(`(?m)^\s*AgentVersion\s*=\s*([0-9]+)\b`)

// agentVersion reads the device protocol version from the source tree being released (not from this tool's own
// build), so that releasing an older tag with -source stays correct.
func agentVersion(source string) (int, error) {
	b, err := os.ReadFile(source + "/internal/proto/wire.go")
	if err != nil {
		return 0, err
	}
	m := agentVersionLine.FindSubmatch(b)
	if m == nil {
		return 0, errors.New("AgentVersion not found in internal/proto/wire.go")
	}
	return strconv.Atoi(string(m[1]))
}

// shellInstaller returns a self-extracting sh installer: the header with the payload's size and digest, then the
// payload and the manifest. The header's own size is written last into a field of fixed width.
func shellInstaller(kind, ver string, payload []byte, m installer.Manifest) ([]byte, error) {
	h := strings.NewReplacer("@KIND@", kind, "@VERSION@", ver, "@PAYLOAD_SIZE@", fmt.Sprintf("%-10d", len(payload)),
		"@PAYLOAD_SHA256@", sha256Hex(payload)).Replace(shellHeader)
	const field = "@HEADER_SIZE@"
	if strings.Count(h, field) != 1 || len(placeholder.FindAllString(h, -1)) != 1 {
		return nil, errors.New("installer.sh has an unexpected placeholder")
	}
	const width = 10
	n := len(h) - len(field) + width
	h = strings.Replace(h, field, fmt.Sprintf("%-*d", width, n), 1)
	if len(h) != n {
		return nil, errors.New("installer header size changed")
	}
	return installer.Assemble([]byte(h), payload, m, nil)
}

// buildInstallers assembles the installers in the order the design requires: the client packages first, then the
// server installer that carries them.
func buildInstallers(ver string, agent int, b binaries) (installers, error) {
	if !version.Valid(ver) {
		return nil, fmt.Errorf("installers need a MAJOR.MINOR.PATCH version, got %q", ver)
	}
	manifest := func(os string) installer.Manifest {
		return installer.Manifest{Version: ver, AgentVersion: agent, OS: os, Arch: "amd64"}
	}
	out := installers{}
	payload, err := installer.Payload([]installer.File{{Name: "kaze-syncc", Data: b.Syncc}})
	if err != nil {
		return nil, err
	}
	linuxClient, err := shellInstaller("client", ver, payload, manifest("linux"))
	if err != nil {
		return nil, err
	}
	winClient, err := installer.Build(b.WinSetup, []installer.File{{Name: "kaze-syncc.exe", Data: b.WinSyncc}, {Name: "kaze-syncc-agent.exe", Data: b.WinAgent}}, manifest("windows"), nil)
	if err != nil {
		return nil, err
	}
	payload, err = installer.Payload([]installer.File{{Name: "kaze-syncd", Data: b.Syncd},
		{Name: clientLinuxPackage, Data: linuxClient}, {Name: clientWindowsPackage, Data: winClient}})
	if err != nil {
		return nil, err
	}
	server, err := shellInstaller("server", ver, payload, manifest("linux"))
	if err != nil {
		return nil, err
	}
	prefix := "kaze-sync-%s-" + ver + "-%s-amd64"
	out[fmt.Sprintf(prefix, "client", "linux")+".sh"] = linuxClient
	out[fmt.Sprintf(prefix, "client", "windows")+"-setup.exe"] = winClient
	out[fmt.Sprintf(prefix, "server", "linux")+".sh"] = server
	return out, nil
}

func sha256Hex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// checkSubsystem fails unless the PE file at path has the wanted subsystem.
func checkSubsystem(path string, want uint16) error {
	f, err := pe.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var got uint16
	switch h := f.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		got = h.Subsystem
	case *pe.OptionalHeader32:
		got = h.Subsystem
	default:
		return fmt.Errorf("%s has no PE optional header", path)
	}
	if got != want {
		return fmt.Errorf("%s has PE subsystem %d, want %d", path, got, want)
	}
	return nil
}
