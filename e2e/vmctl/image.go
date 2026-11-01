package vmctl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"kaze-sync/e2e/config"
)

// The official OpenWrt image the test VM is made from, and that sysupgrade installs.
const (
	OpenWrtVersion = "25.12.6"
	baseURL        = "https://downloads.openwrt.org/releases/" + OpenWrtVersion + "/targets/x86/64/"
	ImageFile      = "openwrt-" + OpenWrtVersion + "-x86-64-generic-ext4-combined-efi.img.gz"
)

// Image returns the path of the official image in the cache directory, downloading and verifying it when absent.
func Image(cfg *config.Config) (string, error) {
	dst := filepath.Join(cfg.Cache, ImageFile)
	sums, err := get(baseURL + "sha256sums")
	if err != nil {
		return "", err
	}
	var want string
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 && strings.TrimPrefix(f[1], "*") == ImageFile {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s not in sha256sums", ImageFile)
	}
	if sum(dst) != want {
		b, err := get(baseURL + ImageFile)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(cfg.Cache, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return "", err
		}
		if got := sum(dst); got != want {
			return "", fmt.Errorf("sha256 mismatch for %s: %s", ImageFile, got)
		}
	}
	return dst, nil
}

func sum(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
