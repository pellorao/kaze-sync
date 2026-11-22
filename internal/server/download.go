package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"kaze-sync/internal/installer"
	"kaze-sync/internal/proto"
	"kaze-sync/internal/version"
)

func (s *Server) packageFor(platform string) (*clientPackage, error) {
	name := clientPackageFilename(platform)
	if name == "" {
		return nil, errors.New("unsupported client platform")
	}
	p, err := openClientPackage(filepath.Join(filepath.Dir(s.varDir), downloadDir, name))
	if err == nil && p.manifest.OS != platform {
		p.Close()
		return nil, errors.New("installer platform does not match its filename")
	}
	return p, err
}

func (s *Server) checkClientPackages() {
	for _, platform := range []string{"linux", "windows"} {
		p, err := s.packageFor(platform)
		if err == nil {
			p.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("client installer unavailable", "platform", platform, "err", err)
		}
	}
}

func packageHeaders(w http.ResponseWriter, p *clientPackage, size int64) {
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename="+clientPackageFilename(p.manifest.OS))
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(proto.PackageVersionHeader, p.manifest.Version)
	h.Set(proto.PackageAgentVersionHeader, strconv.Itoa(p.manifest.AgentVersion))
}

func (s *Server) devicePackage(w http.ResponseWriter, r *http.Request, _ device) {
	w.Header().Set("Cache-Control", "no-store")
	platform := r.URL.Query().Get("os")
	if r.Method == http.MethodHead && platform == "" {
		w.WriteHeader(http.StatusOK) // compatibility probe, independent of package availability
		return
	}
	p, err := s.packageFor(platform)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer p.Close()
	packageHeaders(w, p, p.size)
	if r.Method == http.MethodGet {
		io.CopyN(w, p.File, p.size)
	}
}

// adminDownload serves a client installer of dl/ as it is to a signed-in admin, so that an enrolled device can be
// upgraded from the browser (design 9.5 "管理页下载"). A request without a session is a failed authentication, like
// a wrong enrollment code; a session is not subject to the lockout, as on the other admin pages.
func (s *Server) adminDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.hasSession(r) {
		if s.locked(r) {
			http.Error(w, "too many failures", http.StatusTooManyRequests)
			return
		}
		s.authFailed(r, "client installer download")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p, err := s.packageFor(r.PathValue("os"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer p.Close()
	packageHeaders(w, p, p.size)
	w.Header().Set("Content-Disposition", "attachment; filename="+releaseFilename(p.manifest))
	if r.Method == http.MethodGet {
		io.CopyN(w, p.File, p.size)
	}
}

// releaseFilename is the name of the package on the releases page; ReadManifest has checked the version.
func releaseFilename(m installer.Manifest) string {
	name := "kaze-sync-client-" + m.Version + "-" + m.OS + "-" + m.Arch
	if m.OS == "windows" {
		return name + "-setup.exe"
	}
	return name + ".sh"
}

// clientDownloads describes the packages of dl/ for the download dialog and the upgrade hint (design 4.2). An empty
// Version means that dl/ has no usable package of that system.
type clientDownloads struct{ Linux, Windows clientDownload }

type clientDownload struct{ Version, File string }

func (s *Server) clientDownloads() clientDownloads {
	var d clientDownloads
	for platform, out := range map[string]*clientDownload{"linux": &d.Linux, "windows": &d.Windows} {
		if p, err := s.packageFor(platform); err == nil {
			*out = clientDownload{Version: p.manifest.Version, File: releaseFilename(p.manifest)}
			p.Close()
		}
	}
	return d
}

// upgradable reports whether dl/ holds a newer package for a device of system osName that reported release.
func (d clientDownloads) upgradable(osName, release string) bool {
	pkg := map[string]string{"linux": d.Linux.Version, "windows": d.Windows.Version}[osName]
	if pkg == "" || !version.Valid(release) {
		return false
	}
	n, err := version.Compare(pkg, release)
	return err == nil && n > 0
}

func (s *Server) downloadRegistration(r *http.Request, code, name string) ([]byte, error) {
	return installer.MarshalTrailer(installer.Registration{
		Server: "https://" + r.Host, Fingerprint: s.Fingerprint, Code: code, Name: name,
	}, installer.RegistrationMagic)
}

func (s *Server) enrollmentDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.locked(r) {
		http.Error(w, "too many failures", http.StatusTooManyRequests)
		return
	}
	code, platform, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")
	key := sha(normalizeSecret(code))
	s.mu.Lock()
	pc, ok := s.codes[key]
	if ok && s.now().After(pc.expires) {
		delete(s.codes, key)
		ok = false
	}
	s.mu.Unlock()
	if !ok || clientPackageFilename(platform) == "" {
		s.authFailed(r, "client installer download")
		http.NotFound(w, r)
		return
	}
	p, err := s.packageFor(platform)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer p.Close()
	reg, err := s.downloadRegistration(r, code, pc.name)
	if err != nil {
		http.Error(w, "invalid installer registration", http.StatusBadRequest)
		return
	}
	packageHeaders(w, p, enrollmentDownloadSize(p, reg))
	name := "kaze-sync-client.sh"
	if platform == "windows" {
		name = "kaze-sync-setup.exe"
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	if r.Method == http.MethodGet {
		writeEnrollmentDownload(w, p, reg)
	}
}

// Installs only after the complete here document and function definition reach sh.
// The original installer still runs from a file so it can seek to its trailers.
const pipeStart = `#!/bin/sh
kaze_install() (
    umask 077
    work=$(mktemp -d) || exit 1
    trap 'rm -rf "$work"' 0
    trap 'exit 1' HUP INT TERM
    base64 -d >"$work/installer.sh" <<'KAZE_INSTALLER' || exit 1
`

const pipeEnd = `KAZE_INSTALLER
    sh "$work/installer.sh" "$@"
)
kaze_install "$@"
`

const pipeChunk = 3 * 1024 // a multiple of three so only the final encoded line can have padding

func enrollmentDownloadSize(p *clientPackage, reg []byte) int64 {
	n := p.size + int64(len(reg))
	if p.manifest.OS == "linux" {
		return int64(len(pipeStart)+len(pipeEnd)) + 4*((n+2)/3) + (n+pipeChunk-1)/pipeChunk
	}
	return n
}

func writeEnrollmentDownload(w io.Writer, p *clientPackage, reg []byte) error {
	src := io.MultiReader(io.NewSectionReader(p.File, 0, p.size), bytes.NewReader(reg))
	if p.manifest.OS == "windows" {
		_, err := io.CopyN(w, src, p.size+int64(len(reg)))
		return err
	}
	if _, err := io.WriteString(w, pipeStart); err != nil {
		return err
	}
	var raw [pipeChunk]byte
	var line [pipeChunk/3*4 + 1]byte
	left := p.size + int64(len(reg))
	for left > 0 {
		n := int(min(left, pipeChunk))
		if _, err := io.ReadFull(src, raw[:n]); err != nil {
			return err
		}
		end := base64.StdEncoding.EncodedLen(n)
		base64.StdEncoding.Encode(line[:end], raw[:n])
		line[end] = '\n'
		if _, err := w.Write(line[:end+1]); err != nil {
			return err
		}
		left -= int64(n)
	}
	_, err := io.WriteString(w, pipeEnd)
	return err
}

// enrollmentDownloadInfo is supplied to the enrollment dialog template. Empty fields denote unavailable packages.
type enrollmentDownloadInfo struct {
	Linux, Windows enrollmentPackageInfo
	PublicKeyPin   string
}

type enrollmentPackageInfo struct {
	URL, Version, SHA256 string // SHA256 is the personalized Linux response, for wget
}

type enrollmentChecksum struct {
	file         os.FileInfo
	registration [sha256.Size]byte
	sum          string
}

func (s *Server) enrollmentLinuxSHA256(code string, p *clientPackage, reg []byte) (string, error) {
	key, regHash := sha(normalizeSecret(code)), sha256.Sum256(reg)
	s.mu.Lock()
	pc := s.codes[key]
	s.mu.Unlock()
	if c := pc.linuxHash; c != nil && c.registration == regHash && os.SameFile(c.file, p.info) &&
		c.file.Size() == p.size && c.file.ModTime().Equal(p.info.ModTime()) && !s.now().After(pc.expires) {
		return c.sum, nil
	}
	h := sha256.New()
	if err := writeEnrollmentDownload(h, p, reg); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	if current, ok := s.codes[key]; ok && current.expires.Equal(pc.expires) && !s.now().After(current.expires) {
		current.linuxHash = &enrollmentChecksum{file: p.info, registration: regHash, sum: sum}
		s.codes[key] = current
	}
	s.mu.Unlock()
	return sum, nil
}

func (s *Server) enrollmentDownloads(r *http.Request, code, name string) enrollmentDownloadInfo {
	var info enrollmentDownloadInfo
	reg, err := s.downloadRegistration(r, code, name)
	if err != nil {
		return info
	}
	if p, err := s.packageFor("linux"); err == nil {
		if digest, err := s.enrollmentLinuxSHA256(code, p, reg); err == nil {
			info.Linux = enrollmentPackageInfo{
				URL:     "https://" + r.Host + "/dl/" + code + "/linux",
				Version: p.manifest.Version, SHA256: digest,
			}
			info.PublicKeyPin = s.enrollmentPublicKeyPin()
		}
		p.Close()
	}
	if p, err := s.packageFor("windows"); err == nil {
		info.Windows = enrollmentPackageInfo{URL: "https://" + r.Host + "/dl/" + code + "/windows", Version: p.manifest.Version}
		p.Close()
	}
	return info
}

func (s *Server) enrollmentPublicKeyPin() string {
	if s.Fingerprint == "" || s.serverCertificate == nil {
		return ""
	}
	cert := s.serverCertificate()
	if cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	leaf := cert.Leaf
	if leaf == nil {
		var err error
		leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return ""
		}
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(digest[:])
}
