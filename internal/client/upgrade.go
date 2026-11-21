package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/version"
)

// Tunables of the upgrade.
const (
	maxPackageSize  = 256 << 20 // the client package is about 10 MB; a bigger response is not one
	downloadTimeout = 30 * time.Minute
	upgradeDirGlob  = "upgrade-*" // under the state directory; the next upgrade removes what an earlier one left
)

// UpgradeOptions are the arguments of `kaze-syncc upgrade` (design 9.5).
type UpgradeOptions struct {
	Force bool // install although the server's package is not newer, or its protocol is newer than the server's
	Out   io.Writer
}

// upgradeEnv is what the upgrade needs besides the installation: the running version and, on Windows, how to
// hand the verified installer over to a new process.
type upgradeEnv struct {
	Current string
	Handoff func(installerFile string, force bool) error
}

// Upgrade fetches the client installer from the enrolled server and installs it (design 9.5). Nothing goes to
// any other host.
func Upgrade(ctx context.Context, o UpgradeOptions) error {
	env, err := realInstallEnv()
	if err != nil {
		return err
	}
	return upgrade(ctx, o, env, upgradeEnv{Current: version.Version, Handoff: handoffInstaller})
}

func clientPackageFile(goos string) string {
	if goos == "windows" {
		return "kaze-sync-setup.exe"
	}
	return "kaze-sync-client.sh"
}

func upgrade(ctx context.Context, o UpgradeOptions, env installEnv, up upgradeEnv) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	dev, err := LoadDevice(env.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("this device is not registered: there is no server to upgrade from; use the installer from the Web UI or the release page")
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	resp, err := fetchClientPackage(ctx, dev, env.GOOS)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The header only decides whether to download at all; the version that counts is the one in the verified file.
	if hv := resp.Header.Get(proto.PackageVersionHeader); !o.Force && newerThan(up.Current, hv) <= 0 {
		fmt.Fprintf(out, "already up to date: kaze-syncc %s, the server offers %s\n", up.Current, hv)
		return nil
	}
	for _, old := range globDirs(env.StateDir, upgradeDirGlob) {
		os.RemoveAll(old) // a setup of an earlier Windows upgrade may still hold its file: not fatal
	}
	dir, err := os.MkdirTemp(env.StateDir, "upgrade-")
	if err != nil {
		return err
	}
	file := filepath.Join(dir, clientPackageFile(env.GOOS))
	pkg, err := saveClientPackage(file, resp)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	if pkg.Manifest.OS != env.GOOS || pkg.Manifest.Arch != env.GOARCH {
		os.RemoveAll(dir)
		return fmt.Errorf("the server sent a package for %s/%s, not %s/%s", pkg.Manifest.OS, pkg.Manifest.Arch, env.GOOS, env.GOARCH)
	}
	if !o.Force && newerThan(up.Current, pkg.Manifest.Version) <= 0 {
		os.RemoveAll(dir)
		fmt.Fprintf(out, "already up to date: kaze-syncc %s, the server offers %s\n", up.Current, pkg.Manifest.Version)
		return nil
	}
	fmt.Fprintf(out, "downloaded and verified kaze-syncc %s from the server\n", pkg.Manifest.Version)
	if env.GOOS == "windows" {
		// Windows cannot replace this running program: the installer does it from its own file, in a window of
		// its own, and this process ends so that its file is free. The installer stays in dir until the next upgrade.
		if err := up.Handoff(file, o.Force); err != nil {
			os.RemoveAll(dir)
			return fmt.Errorf("could not start the installer: %w", err)
		}
		fmt.Fprintln(out, "the installer continues in a new window; this program ends now")
		return nil
	}
	defer os.RemoveAll(dir)
	return install(ctx, InstallOptions{From: file, Force: o.Force, Out: out, Temporary: true}, env)
}

// newerThan orders the offered version against the running one: 1 when the offer is newer, 0 when equal, -1
// when older. A version that does not parse counts as "newer", so the file's own manifest decides.
func newerThan(current, offered string) int {
	n, err := version.Compare(offered, current)
	if err != nil {
		return 1
	}
	return n
}

func globDirs(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return m
}

// fetchClientPackage asks the enrolled server for the client package of this system with the device token.
func fetchClientPackage(ctx context.Context, dev Device, goos string) (*http.Response, error) {
	hc, err := NewHTTPClient(dev.Fingerprint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(dev.Server, "/")+"/api/v1/client/package?os="+goos, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+dev.Token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, fmt.Errorf("the server has no client package for %s: install the new client package on the server (kaze-syncd client-pkg) or install this client by hand", goos)
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("the server answered %s", resp.Status)
	}
}

// saveClientPackage writes the response body to file and checks it like any installer: manifest, then payload
// digest. The file is removed by the caller on error.
func saveClientPackage(file string, resp *http.Response) (*installer.Package, error) {
	if resp.ContentLength > maxPackageSize {
		return nil, errors.New("the server's client package is too large")
	}
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxPackageSize+1))
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if n > maxPackageSize {
		return nil, errors.New("the server's client package is too large")
	}
	pkg, err := installer.Open(f, n)
	if err != nil {
		return nil, fmt.Errorf("the downloaded file is not a valid kaze-sync installer: %w", err)
	}
	return pkg, nil
}

// ParseUpgradeArgs parses the arguments of `kaze-syncc upgrade`: [--force].
func ParseUpgradeArgs(args []string) (UpgradeOptions, error) {
	var o UpgradeOptions
	for _, a := range args {
		if a != "--force" && a != "-force" {
			return o, fmt.Errorf("upgrade takes only --force, not %q", a)
		}
		o.Force = true
	}
	return o, nil
}
