package client

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/version"
)

// Tunables of the installation.
const (
	maxBinarySize   = 200 << 20 // a gzip bomb in a tampered installer stops here
	renameTries     = 20        // Windows may hold a just-stopped program's file for a moment
	renameWait      = 100 * time.Millisecond
	probeTimeout    = 10 * time.Second
	versionTimeout  = 5 * time.Second
	oldSuffix       = ".old"
	newSuffix       = ".new"
	linuxSystemUnit = "kaze-sync.service"
)

// InstallOptions are the arguments of `kaze-syncc install` (design 9.5).
type InstallOptions struct {
	From        string // the installer file: its payload is what gets installed
	Dir         string // target directory; empty: where the autostart record points, else the default
	Force       bool   // install although the server speaks an older protocol than this package
	Server      string // manual registration (same arguments as enroll); empty: use the file's registration block
	Code        string
	Fingerprint string
	Out         io.Writer
	Temporary   bool // From is a file the caller deletes itself: do not tell the user to
	// Prompt asks for a registration when the machine is not enrolled and nothing came with the installer (an
	// interactive setup program); nil prints a hint instead.
	Prompt func() (server, code, fingerprint string, ok bool)
}

// installEnv holds everything the installation touches outside its own directory, so tests can replace it.
type installEnv struct {
	GOOS, GOARCH string
	StateDir     string
	AutostartDir func() (string, error) // "" when there is no record
	DefaultDir   func() (string, error) // where a first installation goes
	StopAgent    func() error           // before the files change
	Autostart    func(exe string) error // (re)writes the record and starts the agent
	Enroll       func(ctx context.Context, server, code, fingerprint string) error
	OldVersion   func(dir string) string                            // "" when unknown
	ServerAgent  func(ctx context.Context, dev Device) (int, error) // the server's protocol version
	Linger       func() (enabled, known bool)
	Preflight    func() error // nil: nothing to check; an error stops the installation before anything changes
	Sleep        func(time.Duration)
	Self         func() string // the running program, which Windows cannot replace
}

func realInstallEnv() (installEnv, error) {
	state, err := StateDir()
	if err != nil {
		return installEnv{}, err
	}
	return installEnv{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, StateDir: state,
		AutostartDir: AutostartDir,
		DefaultDir:   defaultInstallDir,
		StopAgent:    func() error { return stopAgentForInstall(state) },
		Autostart:    ConfigureAutostart,
		Enroll:       func(ctx context.Context, server, code, fp string) error { return Enroll(ctx, state, server, code, fp) },
		OldVersion:   installedVersion,
		ServerAgent:  serverAgentVersion,
		Linger:       lingerEnabled,
		Preflight:    preflight,
		Sleep:        time.Sleep,
		Self:         func() string { p, _ := os.Executable(); return p },
	}, nil
}

// Install installs the client from an installer file (design 9.5): the same path for a first installation and an
// upgrade. device.json and the folder databases are never touched.
func Install(ctx context.Context, o InstallOptions) error {
	env, err := realInstallEnv()
	if err != nil {
		return err
	}
	return install(ctx, o, env)
}

// preflight refuses a Linux machine without systemd: the agent runs as a systemd user unit and there is no other
// supported way to run it (design 9.5), so installing would only leave a registered device with no agent.
func preflight() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("the Linux client needs systemd (systemctl was not found); other init systems are not supported, nothing was installed")
	}
	return nil
}

func payloadNames(goos string) []string {
	if goos == "windows" {
		return []string{"kaze-syncc.exe", "kaze-syncc-agent.exe"}
	}
	return []string{"kaze-syncc"}
}

func defaultInstallDir() (string, error) {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, "Programs", "kaze-sync"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func install(ctx context.Context, o InstallOptions, env installEnv) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	if o.From == "" {
		return errors.New("install needs --from FILE, the installer to install from")
	}
	f, err := os.Open(o.From)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	pkg, err := installer.Open(f, fi.Size())
	if err != nil {
		return fmt.Errorf("%s is not a kaze-sync installer: %w", o.From, err)
	}
	if pkg.Manifest.OS != env.GOOS || pkg.Manifest.Arch != env.GOARCH {
		return fmt.Errorf("this installer is for %s/%s, not %s/%s", pkg.Manifest.OS, pkg.Manifest.Arch, env.GOOS, env.GOARCH)
	}
	if env.Preflight != nil {
		if err := env.Preflight(); err != nil {
			return err
		}
	}
	dev, err := LoadDevice(env.StateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	enrolled := err == nil
	if enrolled && !o.Force {
		if err := checkCompatible(ctx, dev, pkg.Manifest, env, out); err != nil {
			return err
		}
	}
	autostartDir, err := env.AutostartDir()
	if err != nil {
		return err
	}
	dir := o.Dir
	if dir == "" {
		dir = autostartDir
	}
	if dir == "" {
		if dir, err = env.DefaultDir(); err != nil {
			return err
		}
	}
	names := payloadNames(env.GOOS)
	if env.GOOS == "windows" && env.Self != nil {
		self := env.Self()
		for _, n := range names {
			if strings.EqualFold(filepath.Clean(self), filepath.Join(dir, n)) {
				return fmt.Errorf("%s is running and Windows cannot replace a running program: run the installer (kaze-sync-setup.exe) instead", self)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old := env.OldVersion(dir)
	staged, err := stagePayload(pkg.Payload(f), dir, names)
	if err != nil {
		return err
	}
	if err := env.StopAgent(); err != nil {
		removeAll(staged)
		return fmt.Errorf("could not stop the running agent: %w", err)
	}
	if err := replaceFiles(dir, names, staged, env); err != nil {
		if autostartDir != "" { // the agent was meant to run: bring the old one back
			env.Autostart(filepath.Join(autostartDir, names[0]))
		}
		return err
	}
	if old == "" {
		old = "none"
	}
	fmt.Fprintf(out, "kaze-syncc %s -> %s in %s\nreplaced: %s\n", old, pkg.Manifest.Version, dir, strings.Join(names, ", "))
	reg, enrollErr := registration(o, pkg, enrolled)
	if enrollErr == nil && reg == nil && !enrolled && o.Prompt != nil {
		if s, c, fp, ok := o.Prompt(); ok {
			reg = &installer.Registration{Server: s, Code: c, Fingerprint: fp}
			enrollErr = reg.Validate()
		}
	}
	if enrollErr == nil && reg != nil {
		if enrollErr = env.Enroll(ctx, reg.Server, reg.Code, reg.Fingerprint); enrollErr == nil {
			enrolled = true
			fmt.Fprintln(out, "enrolled this device")
		}
	}
	if enrollErr != nil {
		fmt.Fprintln(out, "the program files are installed, but this device is not registered: add the device again in the Web UI and use its new command or link")
		return fmt.Errorf("registration failed: %w", enrollErr)
	}
	if !enrolled {
		fmt.Fprintf(out, "not registered yet: run the registration command from the Web UI (\"Add device\"), kaze-syncc enroll SERVER CODE [--fingerprint F], from %s\n", dir)
		return nil
	}
	if err := env.Autostart(filepath.Join(dir, names[0])); err != nil {
		return fmt.Errorf("installed, but starting the agent failed: %w", err)
	}
	fmt.Fprintln(out, "the agent is running and starts at login")
	if env.GOOS == "linux" {
		if on, known := env.Linger(); known && !on {
			fmt.Fprintln(out, "on a machine without a graphical session the agent stops when your last session ends; run as root: loginctl enable-linger "+currentUserName())
		}
	}
	if !o.Temporary {
		fmt.Fprintf(out, "you can delete %s now\n", o.From)
	}
	return nil
}

// registration picks what to enroll with: explicit arguments always; the installer's own block only on a machine
// that is not enrolled yet (an old downloaded installer must not replace a working registration). Both are
// validated like any untrusted input.
func registration(o InstallOptions, pkg *installer.Package, enrolled bool) (*installer.Registration, error) {
	var reg *installer.Registration
	switch {
	case o.Server != "":
		reg = &installer.Registration{Server: o.Server, Code: o.Code, Fingerprint: o.Fingerprint}
	case pkg.Registration != nil && !enrolled:
		reg = pkg.Registration
	default:
		return nil, nil
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return reg, nil
}

// checkCompatible refuses a package whose protocol is newer than the server's. The check is on the protocol
// version, not the release number, so a client-only upgrade passes. A server that cannot be reached only warns.
func checkCompatible(ctx context.Context, dev Device, m installer.Manifest, env installEnv, out io.Writer) error {
	av, err := env.ServerAgent(ctx, dev)
	if err != nil {
		fmt.Fprintln(out, "warning: could not ask the server for its version, not checking:", err)
		return nil
	}
	if m.AgentVersion > av {
		return fmt.Errorf("this client (release %s, protocol %d) is newer than the server (protocol %d): upgrade the server first, or use --force", m.Version, m.AgentVersion, av)
	}
	return nil
}

func serverAgentVersion(ctx context.Context, dev Device) (int, error) {
	hc, err := NewHTTPClient(dev.Fingerprint)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimRight(dev.Server, "/")+"/api/v1/client/package", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+dev.Token)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return proto.LastAgentVersionWithoutHeader, nil // a server older than this route
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("server answered %s", resp.Status)
	}
	h := resp.Header.Get(proto.ServerAgentVersionHeader)
	if h == "" {
		return proto.LastAgentVersionWithoutHeader, nil // a server older than the header
	}
	return strconv.Atoi(h)
}

// stagePayload writes each wanted binary of the payload next to its final place under a temporary name. Nothing
// else of the archive is looked at or written: the archive comes from a downloaded file.
func stagePayload(payload io.Reader, dir string, names []string) (map[string]string, error) {
	gz, err := gzip.NewReader(payload)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	staged := map[string]string{}
	fail := func(err error) (map[string]string, error) { removeAll(staged); return nil, err }
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		name := path.Base(h.Name)
		if h.Typeflag != tar.TypeReg || !slices.Contains(names, name) {
			continue
		}
		if staged[name] != "" {
			return fail(fmt.Errorf("installer holds %s twice", name))
		}
		f, err := os.CreateTemp(dir, name+"-*"+newSuffix) // unique: another install may run in this directory
		if err != nil {
			return fail(err)
		}
		staged[name] = f.Name()
		if err := writeBinary(f, tr); err != nil {
			return fail(err)
		}
	}
	for _, n := range names {
		if staged[n] == "" {
			return fail(fmt.Errorf("installer does not contain %s", n))
		}
	}
	return staged, nil
}

// writeBinary fills the just created temporary file f, makes it executable and closes it.
func writeBinary(f *os.File, r io.Reader) error {
	n, err := io.Copy(f, io.LimitReader(r, maxBinarySize+1))
	if err == nil {
		err = f.Chmod(0o755)
	}
	if err == nil && n > maxBinarySize {
		err = errors.New("a binary in the installer is too large")
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func removeAll(staged map[string]string) {
	for _, p := range staged {
		os.Remove(p)
	}
}

// replaceFiles moves the old files aside, then the new ones into place. A failure in the middle puts every moved
// file back, so the directory holds a complete old or a complete new version.
func replaceFiles(dir string, names []string, staged map[string]string, env installEnv) error {
	var moved []string // names whose old file is now name+oldSuffix
	var placed []string
	rollback := func() {
		for _, n := range placed {
			os.Remove(filepath.Join(dir, n))
		}
		for _, n := range moved {
			os.Rename(filepath.Join(dir, n+oldSuffix), filepath.Join(dir, n))
		}
		removeAll(staged)
	}
	for _, n := range names {
		final := filepath.Join(dir, n)
		if _, err := os.Lstat(final); err == nil {
			if err := renameRetry(final, final+oldSuffix, env); err != nil {
				rollback()
				return fmt.Errorf("could not replace %s: %w", final, err)
			}
			moved = append(moved, n)
		}
		if err := renameRetry(staged[n], final, env); err != nil {
			rollback()
			return fmt.Errorf("could not replace %s: %w", final, err)
		}
		placed = append(placed, n)
	}
	for _, n := range moved {
		os.Remove(filepath.Join(dir, n+oldSuffix))
	}
	return nil
}

func renameRetry(from, to string, env installEnv) error {
	tries := 1
	if env.GOOS == "windows" {
		tries = renameTries
	}
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 {
			env.Sleep(renameWait)
		}
		if err = os.Rename(from, to); err == nil {
			return nil
		}
	}
	return err
}

// stopAgentForInstall stops the agent before its files change. On Linux the user unit goes first: with Restart= a
// bare kill would bring the old version back. StopAgent then ends an agent that was started by hand.
func stopAgentForInstall(state string) error {
	if runtime.GOOS == "linux" {
		if cfg, err := os.UserConfigDir(); err == nil {
			if _, err := os.Stat(filepath.Join(cfg, "systemd", "user", linuxSystemUnit)); err == nil {
				exec.Command("systemctl", "--user", "stop", linuxSystemUnit).Run() // not fatal: StopAgent below still runs
			}
		}
	}
	return StopAgent(state)
}

// installedVersion asks the installed program for its version; "" when it is missing or does not answer.
func installedVersion(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(dir, payloadNames(runtime.GOOS)[0]), "--version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) == 2 && version.Valid(f[1]) {
		return f[1]
	}
	return ""
}

func currentUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "$USER"
}

// lingerEnabled reports whether systemd keeps the user's services running after the last session ends.
func lingerEnabled() (enabled, known bool) {
	out, err := exec.Command("loginctl", "show-user", currentUserName(), "-p", "Linger").Output()
	if err != nil {
		return false, false
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "Linger=")
	return v == "yes", ok
}

// quotedCommandPath returns the program path of an autostart command line such as `"C:\a b\x.exe" run`.
func quotedCommandPath(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if rest, ok := strings.CutPrefix(cmd, `"`); ok {
		p, _, _ := strings.Cut(rest, `"`)
		return p
	}
	p, _, _ := strings.Cut(cmd, " ")
	return p
}

// unitExecDir is the directory of the ExecStart program of a unit file.
func unitExecDir(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		if cmd, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			if p := quotedCommandPath(cmd); p != "" {
				return filepath.Dir(p)
			}
		}
	}
	return ""
}
