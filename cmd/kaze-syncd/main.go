// Command kaze-syncd is the kaze-sync server: run, install, admin-password, export.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"kaze-sync/internal/server"
)

const usage = `usage:
  kaze-syncd run --data DIR
  kaze-syncd install --data DIR [--listen :PORT] [--cert FILE --key FILE] [--wan]
  kaze-syncd admin-password [--data DIR]
  kaze-syncd export DIR [--data DIR] [--folder NAME|ID] [--at "YYYY-MM-DD HH:MM[:SS]" (UTC)]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	exe, _ := os.Executable()
	def := "" // run and install need an explicit directory; admin-password defaults to the binary's
	if cmd == "admin-password" || cmd == "export" {
		def = filepath.Dir(exe)
	}
	data := fs.String("data", def, "data directory")
	var o server.InstallOptions
	if cmd == "install" {
		fs.StringVar(&o.Listen, "listen", "", "listen address, kept from the first install when omitted")
		fs.StringVar(&o.Cert, "cert", "", "certificate file (instead of the self-signed one)")
		fs.StringVar(&o.Key, "key", "", "private key file")
		fs.BoolVar(&o.WAN, "wan", false, "open the port on the WAN zone of the firewall")
	}
	var export server.ExportOptions
	var at string
	if cmd == "export" {
		fs.StringVar(&export.Folder, "folder", "", "export only this folder (name or id) directly into DIR")
		fs.StringVar(&at, "at", "", "the state at this time in UTC (default: now)")
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") { // DIR comes first
			export.Dir, args = args[0], args[1:]
		}
	}
	fs.Parse(args)
	if *data == "" && (cmd == "run" || cmd == "install") {
		fmt.Fprintln(os.Stderr, "--data is required")
		os.Exit(2)
	}

	var err error
	switch cmd {
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = server.Run(ctx, *data)
	case "install":
		o.Data = *data
		err = server.NewInstaller().Install(o)
	case "export":
		export.Data = *data
		if export.Dir == "" || fs.NArg() > 0 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		if at != "" {
			if export.At, err = server.ParseTime(at); err != nil {
				break
			}
		}
		err = server.NewInstaller().Export(export)
	case "admin-password":
		err = server.NewInstaller().AdminPassword(*data)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error(cmd, "err", err)
		os.Exit(1)
	}
}
