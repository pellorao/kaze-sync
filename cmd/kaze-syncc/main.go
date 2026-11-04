// Command kaze-syncc is the kaze-sync client: enroll and run.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"kaze-sync/internal/client"
	cfs "kaze-sync/internal/client/fs"
)

const usage = `usage:
  kaze-syncc enroll SERVER CODE [--fingerprint sha256:HEX] [--no-autostart]
  kaze-syncc run`

// Tunables.
const (
	scanInterval = time.Hour
	retryAfter   = 10 * time.Second
	stablePeriod = 5 * time.Second
	maxLogSize   = 10 << 20
	debugMarker  = "debug" // a file of this name in the state directory turns on debug logging
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	dir, err := client.StateDir()
	if err == nil {
		switch os.Args[1] {
		case "enroll":
			err = enroll(dir, os.Args[2:])
		case "run":
			err = run(dir)
		default:
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kaze-syncc:", err)
		os.Exit(1)
	}
}

func enroll(dir string, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	fp := fs.String("fingerprint", "", "pinned certificate fingerprint (self-signed servers)")
	noAuto := fs.Bool("no-autostart", false, "do not configure autostart")
	// Flags may follow the positional arguments.
	var pos []string
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	if len(pos) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := client.Enroll(context.Background(), dir, pos[0], pos[1], *fp); err != nil {
		return err
	}
	fmt.Println("enrolled; settings are in", dir)
	if *noAuto {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return client.ConfigureAutostart(exe)
}

func run(dir string) error {
	dev, err := client.LoadDevice(dir)
	if err != nil {
		return fmt.Errorf("not enrolled (run enroll first): %w", err)
	}
	logFile := filepath.Join(dir, "kaze-syncc.log")
	if fi, err := os.Stat(logFile); err == nil && fi.Size() > maxLogSize {
		os.Rename(logFile, logFile+".old")
	}
	lf, err := os.OpenFile(logFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	opts := &slog.HandlerOptions{}
	if _, err := os.Stat(filepath.Join(dir, debugMarker)); err == nil { // `touch` it and restart the agent to log the timeline
		opts.Level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(lf, opts)))

	hc, err := client.NewHTTPClient(dev.Fingerprint)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	a := &client.Agent{
		Dir: dir, API: &client.API{Base: dev.Server, Token: dev.Token, HC: hc}, NewFS: cfs.OS,
		OS: runtime.GOOS, Hostname: host, DeviceName: dev.Name, Stable: stablePeriod, ScanInterval: scanInterval, RetryAfter: retryAfter,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("agent started", "server", dev.Server)
	return a.Run(ctx)
}
