package server

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
)

const downloadDir = "dl"

func clientPackageFilename(platform string) string {
	switch platform {
	case "linux":
		return "client-linux-amd64.sh"
	case "windows":
		return "client-windows-amd64-setup.exe"
	}
	return ""
}

type clientPackage struct {
	*os.File
	manifest         installer.Manifest
	info             os.FileInfo
	size, payloadEnd int64
}

func openClientPackage(path string) (*clientPackage, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("installer is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err = f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("installer is not a regular file")
	}
	if err == nil {
		m, end, readErr := installer.ReadManifest(f, fi.Size())
		switch {
		case readErr != nil:
			err = readErr
		case clientPackageFilename(m.OS) == "" || m.Arch != "amd64":
			err = errors.New("installer must be for linux/amd64 or windows/amd64")
		default:
			return &clientPackage{File: f, manifest: m, info: fi, size: fi.Size(), payloadEnd: end}, nil
		}
	}
	f.Close()
	return nil, err
}

// verifyPayload verifies the compressed bytes up to the manifest against its digest and checks their kind.
func (p *clientPackage) verifyPayload() error {
	start, err := installer.FindPayload(p.File, p.payloadEnd, p.manifest.PayloadSHA256)
	if err != nil {
		return err
	}
	return p.checkPayloadKind(start)
}

// The Linux server and client installers have the same platform in their manifests.
// Their first payload entries distinguish them without unpacking the whole archive.
func (p *clientPackage) checkPayloadKind(start int64) error {
	gz, err := gzip.NewReader(io.NewSectionReader(p.File, start, p.payloadEnd-start))
	if err != nil {
		return err
	}
	defer gz.Close()
	first, err := tar.NewReader(gz).Next()
	if err != nil {
		return fmt.Errorf("installer payload is not a tar archive: %w", err)
	}
	name := path.Base(first.Name)
	if first.Typeflag == tar.TypeReg && (p.manifest.OS == "linux" && name == "kaze-syncc" ||
		p.manifest.OS == "windows" && (name == "kaze-syncc.exe" || name == "kaze-syncc-agent.exe")) {
		return nil
	}
	return errors.New("installer payload is not a client installer for the declared platform")
}

// ClientPackages publishes complete replacements without restarting the server. All inputs are checked before
// publishing any of them. Each rename leaves an in-progress download reading its original open file.
func (in *Installer) ClientPackages(data string, paths []string, force bool) error {
	if len(paths) == 0 || len(paths) > 2 {
		return errors.New("provide one installer per client platform")
	}
	if data == "" {
		svc, err := in.detect()
		if err != nil {
			return err
		}
		data, err = in.installedData(svc)
		if err != nil {
			return err
		}
	}
	data = filepath.Clean(data)
	if !safePath.MatchString(data) {
		return errors.New("--data must be an absolute path of letters, digits and ._-/")
	}
	dbPath := filepath.Join(data, VarDir, DBFile)
	if fi, err := os.Stat(dbPath); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("no installed server database at %s", dbPath)
	}
	lock, err := lockInstall(data)
	if err != nil {
		return err
	}
	defer lock.Close()

	type replacement struct{ tmp, dst, platform, version string }
	var staged []replacement
	defer func() {
		for _, f := range staged {
			os.Remove(f.tmp)
		}
	}()
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		p, err := openClientPackage(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		defer p.Close()
		m := p.manifest
		if seen[m.OS] {
			return fmt.Errorf("more than one installer for %s", m.OS)
		}
		seen[m.OS] = true
		if m.AgentVersion > proto.AgentVersion && !force {
			return fmt.Errorf("%s needs agent protocol %d, server supports %d; upgrade the server first or use --force", path, m.AgentVersion, proto.AgentVersion)
		}
		dst := filepath.Join(data, downloadDir, clientPackageFilename(m.OS))
		tmp, err := stageFile(dst, 0o644, func(f *os.File) error {
			if _, err := io.Copy(f, io.NewSectionReader(p.File, 0, p.size)); err != nil {
				return err
			}
			// Verify the copy that will be published, even if the source changed during the copy.
			m, end, err := installer.ReadManifest(f, p.size)
			if err != nil {
				return err
			}
			if m != p.manifest {
				return errors.New("installer changed during copy")
			}
			copied := clientPackage{File: f, manifest: m, payloadEnd: end}
			return copied.verifyPayload()
		})
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		staged = append(staged, replacement{tmp: tmp, dst: dst, platform: m.OS, version: m.Version})
	}
	// The service reads the packages as an unprivileged user, even when root's umask is restrictive.
	if err := os.Chmod(filepath.Join(data, downloadDir), 0o755); err != nil {
		return err
	}
	for _, f := range staged {
		if err := os.Rename(f.tmp, f.dst); err != nil {
			return err
		}
		in.say("published %s client installer %s", f.platform, f.version)
	}
	return nil
}
