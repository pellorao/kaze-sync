// Command kaze-syncc is the kaze-sync client: enroll and run.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"kaze-sync/internal/version"
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
  kaze-syncc run
  kaze-syncc install --from FILE [--dir DIR] [--force] [SERVER CODE [--fingerprint sha256:HEX]]`

// Tunables.
const (
	scanInterval = time.Hour
	retryAfter   = 10 * time.Second
	stablePeriod = 5 * time.Second
	maxLogSize   = 10 << 20
	debugMarker  = "debug" // a file of this name in the state directory turns on debug logging
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		exitUsage("")
	}
	if args[0] == "--version" {
		fmt.Println("kaze-syncc " + version.Version)
		return
	}
	// Help and unknown commands are decided before anything touches the state directory.
	if args[0] == "help" || hasHelp(args) {
		fmt.Println(usage)
		return
	}
	if args[0] != "enroll" && args[0] != "run" && args[0] != "install" {
		exitUsage("unknown command " + args[0])
	}
	dir, err := client.StateDir()
	if err == nil {
		switch args[0] {
		case "enroll":
			err = enroll(dir, args[1:])
		case "install":
			o, perr := client.ParseInstallArgs(args[1:], "")
			if perr != nil {
				exitUsage(perr.Error())
			}
			o.Out = os.Stdout
			err = client.Install(context.Background(), o)
		case "run":
			if len(args) > 1 {
				exitUsage("run takes no arguments")
			}
			err = run(dir)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kaze-syncc:", err)
		os.Exit(1)
	}
}

func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "-help" || a == "--help" {
			return true
		}
	}
	return false
}

// exitUsage prints the problem and the usage to stderr and exits with status 2.
func exitUsage(problem string) {
	if problem != "" {
		fmt.Fprintln(os.Stderr, "kaze-syncc:", problem)
	}
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}

func enroll(dir string, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fp := fs.String("fingerprint", "", "pinned certificate or CA fingerprint (self-signed and local-CA servers)")
	noAuto := fs.Bool("no-autostart", false, "do not configure autostart")
	// Flags may follow the positional arguments.
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			exitUsage(err.Error())
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	if len(pos) != 2 {
		exitUsage("enroll needs SERVER and CODE")
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
	if runtime.GOOS == "windows" { // the Run key only starts agents; a running one keeps the old token
		if err := client.StopAgent(dir); err != nil {
			return err
		}
	}
	return client.ConfigureAutostart(exe)
}

func run(dir string) error {
	dev, err := client.LoadDevice(dir)
	if err != nil {
		return fmt.Errorf("not enrolled (run enroll first): %w", err)
	}
	release, err := client.LockInstance(dir)
	if err != nil {
		return err
	}
	defer release()
	logFile := filepath.Join(dir, "kaze-syncc.log")
	lf, err := openLog(logFile, maxLogSize)
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
	if _, err := os.Stat(filepath.Join(dir, "ntfy.json")); err == nil {
		slog.Info("ntfy.json is not used any more, see the user manual 9.1")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("agent started", "server", dev.Server)
	return a.Run(ctx)
}
