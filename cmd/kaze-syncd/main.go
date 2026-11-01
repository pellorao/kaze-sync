// Command kaze-syncd is the kaze-sync server: run, install, admin-password.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"kaze-sync/internal/server"
)

const usage = `usage:
  kaze-syncd run --data DIR
  kaze-syncd install --data DIR [--listen :PORT] [--cert FILE --key FILE] [--wan]
  kaze-syncd admin-password [--data DIR]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	exe, _ := os.Executable()
	def := "" // run and install need an explicit directory; admin-password defaults to the binary's
	if cmd == "admin-password" {
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
