// Command statecopy makes a SQLite snapshot for diagnosing client folder state.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: statecopy PATH/TO/folder-ID.db")
		os.Exit(2)
	}
	path, err := snapshot(os.TempDir(), os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "statecopy:", err)
		os.Exit(1)
	}
	fmt.Println(path)
}

func snapshot(tempDir, source string) (string, error) {
	if !filepath.IsAbs(tempDir) {
		return "", fmt.Errorf("temporary directory must be absolute: %q", tempDir)
	}
	tempDir, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		return "", err
	}
	for dir := tempDir; ; dir = filepath.Dir(dir) {
		_, err := os.Lstat(filepath.Join(dir, ".syncroot"))
		if err == nil {
			return "", fmt.Errorf("temporary directory is inside sync root %q", dir)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(source), OmitHost: true, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return "", err
	}
	defer db.Close()

	dir, err := os.MkdirTemp(tempDir, "kaze-state-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "state.db")
	// SQLite takes a consistent snapshot, including committed changes still in the WAL.
	if _, err := db.Exec(`VACUUM INTO ?`, path); err != nil {
		return "", errors.Join(err, os.RemoveAll(dir))
	}
	return path, nil
}
