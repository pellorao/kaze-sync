// Command release builds the Linux and Windows release archives without downloading modules.
// Run from the repository root: go run ./tools/release.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var archiveTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

func main() {
	source := flag.String("source", ".", "source tree (also supports an exported tag)")
	out := flag.String("out", "", "output directory (default: <source>/dist/v<version>)")
	flag.Parse()
	if err := release(*source, *out); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func release(source, out string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(source, "internal/version/VERSION"))
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`).MatchString(version) {
		return fmt.Errorf("invalid VERSION: %q", version)
	}
	if out == "" {
		out = filepath.Join(source, "dist", "v"+version)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(".tmp", 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(".tmp", "release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stage, err = filepath.Abs(stage)
	if err != nil {
		return err
	}
	var archives []string
	for _, target := range []string{"linux", "windows"} {
		var names []string
		for _, binary := range []string{"kaze-syncd", "kaze-syncc", "kaze-syncc-agent"} {
			if target == "windows" && binary == "kaze-syncd" || target == "linux" && binary == "kaze-syncc-agent" {
				continue
			}
			pkg := binary
			ldflags := "-s -w -buildid="
			if binary == "kaze-syncc-agent" {
				pkg = "kaze-syncc"
				ldflags += " -H=windowsgui"
			}
			name := binary
			if target == "windows" {
				name += ".exe"
			}
			fmt.Printf("Building %s/amd64 %s\n", target, name)
			cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", filepath.Join(stage, name), "./cmd/"+pkg)
			cmd.Dir = source
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target, "GOARCH=amd64", "GOAMD64=v1", "GOFLAGS=", "GOEXPERIMENT=", "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("build %s: %w", name, err)
			}
			names = append(names, name)
		}
		sort.Strings(names)
		name := "kaze-sync-" + version + "-" + target + "-amd64"
		if target == "linux" {
			name += ".tar.gz"
		} else {
			name += ".zip"
		}
		if err := pack(filepath.Join(stage, name), stage, names, target == "linux"); err != nil {
			return err
		}
		archives = append(archives, name)
	}
	sort.Strings(archives)
	var sums strings.Builder
	for _, name := range archives {
		f, err := os.Open(filepath.Join(stage, name))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintf(&sums, "%x  %s\n", h.Sum(nil), name)
	}
	if err := os.WriteFile(filepath.Join(stage, "SHA256SUMS"), []byte(sums.String()), 0644); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	for _, name := range append(archives, "SHA256SUMS") {
		if err := copyFile(filepath.Join(stage, name), filepath.Join(out, name)); err != nil {
			return err
		}
	}
	fmt.Println("Packages:", out)
	fmt.Printf("gh release create %s --verify-tag --notes-file %s", shellQuote("v"+version), shellQuote(filepath.Join(source, "RELEASE-NOTES.md")))
	for _, name := range append(archives, "SHA256SUMS") {
		fmt.Print(" ", shellQuote(filepath.Join(out, name)))
	}
	fmt.Println()
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if e := out.Close(); err == nil {
			err = e
		}
	}()
	_, err = io.Copy(out, in)
	return err
}

func pack(path, dir string, names []string, linux bool) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
	}()
	var tw *tar.Writer
	var zw *zip.Writer
	if linux {
		gz := gzip.NewWriter(f) // Zero timestamp, empty name and fixed OS byte.
		defer func() {
			if e := gz.Close(); err == nil {
				err = e
			}
		}()
		tw = tar.NewWriter(gz)
		defer func() {
			if e := tw.Close(); err == nil {
				err = e
			}
		}()
	} else {
		zw = zip.NewWriter(f)
		defer func() {
			if e := zw.Close(); err == nil {
				err = e
			}
		}()
	}
	for _, name := range names {
		if err := packFile(tw, zw, filepath.Join(dir, name), name); err != nil {
			return err
		}
	}
	return nil
}

func packFile(tw *tar.Writer, zw *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var w io.Writer
	if tw != nil {
		err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: info.Size(), ModTime: archiveTime, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
		w = tw
	} else {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetModTime(archiveTime)
		h.SetMode(0755)
		w, err = zw.CreateHeader(h)
	}
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}
