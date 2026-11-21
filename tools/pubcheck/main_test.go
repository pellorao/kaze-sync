package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The samples are joined at run time so that this file holds none of the text it looks for.
func j(parts ...string) string { return strings.Join(parts, "") }

func TestScanTextHits(t *testing.T) {
	cases := []struct {
		line, rule string
	}{
		{j("首要目标 (用户 ", "2026-11-02): 低内存"), "user note"},
		{j("决定 (用户", "2027-01-05)"), "user note"},
		{j("不碰 `~/", "projects` 的真实数据"), "local path"},
		{j("see $HOME/", "projects/x"), "local path"},
		{j("登记在 /home/alice/", "projects/.downloads"), "local path"},
		{j("避开 Str", "ata 运行时"), "private project"},
		{j("a ", "qwen-str", "ata run"), "private project"},
		{j("kaze-work", "shop"), "private project"},
		{j("AR", "W-codex2026"), "private project"},
		{j("参照 A", "RW 的做法"), "private project"},
		{j("release-", "vm.mjs"), "private project"},
		{j("跑 restore-", "check.py"), "private project"},
		{j("另开专题 `kaze-sync/", "blocks`"), "internal task name"},
		{j("// kaze-sync/", "cpufix2: findings"), "internal task name"},
		{j("下次发布 (kaze-sync/", "release, 不早于"), "internal task name"},
		{j("kaze-sync/", "bigfile"), "internal task name"},
	}
	for _, c := range cases {
		hits := scanText("x.md", []byte("first line\n"+c.line+"\n"))
		if len(hits) != 1 || hits[0].rule != c.rule || hits[0].line != 2 {
			t.Errorf("%q: got %v, want one %q hit on line 2", c.line, hits, c.rule)
		}
	}
}

func TestScanTextNoHits(t *testing.T) {
	for _, line := range []string{
		"由用户选择数据目录; 用户只有一个",
		"2026-11-04 增加普通 Linux 服务端安装",
		"./kaze-syncd install --data /mnt/data/kaze-sync        # 可选",
		"/mnt/data/kaze-sync/kaze-syncd admin-password",
		"ExecStart=/var/lib/kaze-sync/kaze-syncd run --data /var/lib/kaze-sync",
		"ReadWritePaths=/var/lib/kaze-sync/var",
		"`--data ~/.kaze-sync/server` and `~/.kaze-sync/config/`",
		"Linux `~/.local/state/kaze-sync`, Windows `%LOCALAPPDATA%\\kaze-sync`",
		"import \"kaze-sync/internal/proto\"",
		"https://github.com/example/kaze-sync/releases",
		j("~/", "projectsx is not ~/", "project"),
		"strategy, stratum, ARWEN, warw, harware",
		"a .arw raw photo",
	} {
		if hits := scanText("x.md", []byte(line)); len(hits) != 0 {
			t.Errorf("%q: unexpected hits %v", line, hits)
		}
	}
}

func TestExcerptKeepsRunes(t *testing.T) {
	line := strings.Repeat("写入控制", 20) + j("用户 ", "2026-11-05") + strings.Repeat("冻结", 30)
	hits := scanText("x.md", []byte(line))
	if len(hits) != 1 {
		t.Fatalf("got %v", hits)
	}
	if !strings.Contains(hits[0].text, j("用户 ", "2026-11-05")) || !isValidUTF8(hits[0].text) {
		t.Errorf("bad excerpt %q", hits[0].text)
	}
}

func isValidUTF8(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

func TestScanSkipsBinaryAndIgnoredDirs(t *testing.T) {
	root := t.TempDir() // not a Git repository: the walk is used
	write := func(rel, data string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bad := j("~/", "projects")
	write("a.md", "fine\n")
	write("docs/b.md", "x\n"+bad+"\n")
	write("font.woff2", "\x00\x01"+bad)
	write(".tmp/private.md", bad)
	write(".git/config", bad)
	hits, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].path != "docs/b.md" || hits[0].line != 2 {
		t.Fatalf("got %v", hits)
	}
}

// The repository itself must be clean: this is the regression check that runs with go test ./...
// Over 100 ms (serial run): scans the whole repository; time grows with the repository.
func TestRepositoryIsClean(t *testing.T) {
	hits, err := scan(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Errorf("%v", h)
	}
}
