// Command kaze-syncd is the kaze-sync server: run, install, ca, admin-password, export.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"kaze-sync/internal/server"
	"kaze-sync/internal/version"
)

const usage = `usage:
  kaze-syncd run --data DIR
  kaze-syncd install --data DIR [--listen :PORT] [--cert FILE --key FILE | --ca [--san NAMES]] [--wan] [--new-epoch]
  kaze-syncd ca [--data DIR] [--san NAMES] [--export FILE|-]
  kaze-syncd admin-password [--data DIR]
  kaze-syncd export DIR [--data DIR] [--folder NAME|ID] [--at "YYYY-MM-DD HH:MM[:SS]" (UTC)]`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		exitUsage("")
	}
	if args[0] == "--version" {
		fmt.Println("kaze-syncd " + version.Version)
		return
	}
	// Help and unknown commands are decided before anything is opened.
	if args[0] == "help" || hasHelp(args) {
		fmt.Println(usage)
		return
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "run", "install", "admin-password", "export", "ca":
	default:
		exitUsage("unknown command " + cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	exe, _ := os.Executable()
	def := "" // run and install need an explicit directory; admin-password defaults to the binary's
	if cmd == "admin-password" || cmd == "export" || cmd == "ca" {
		def = filepath.Dir(exe)
	}
	data := fs.String("data", def, "data directory")
	var o server.InstallOptions
	if cmd == "install" {
		fs.StringVar(&o.Listen, "listen", "", "listen address, kept from the first install when omitted")
		fs.StringVar(&o.Cert, "cert", "", "certificate file (instead of the self-signed one)")
		fs.StringVar(&o.Key, "key", "", "private key file")
		fs.BoolVar(&o.CA, "ca", false, "use a local CA and a server certificate signed by it")
		fs.StringVar(&o.SAN, "san", "", "extra host names and IP addresses for the server certificate, comma separated (with --ca)")
		fs.BoolVar(&o.WAN, "wan", false, "open the port on the WAN zone of the firewall")
		fs.BoolVar(&o.NewEpoch, "new-epoch", false, "after restoring a backup: make the clients read everything again")
	}
	var caOpts server.CAOptions
	if cmd == "ca" {
		fs.StringVar(&caOpts.SAN, "san", "", "extra host names and IP addresses for the server certificate, comma separated")
		fs.StringVar(&caOpts.Export, "export", "", "write the CA certificate to this file (- for the output)")
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
	if err := fs.Parse(args); err != nil {
		exitUsage(err.Error())
	}
	if fs.NArg() > 0 {
		exitUsage("unexpected argument " + fs.Arg(0))
	}
	if *data == "" && (cmd == "run" || cmd == "install") {
		exitUsage("--data is required")
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
		if export.Dir == "" {
			exitUsage("export needs DIR")
		}
		if at != "" {
			if export.At, err = server.ParseTime(at); err != nil {
				break
			}
		}
		err = server.NewInstaller().Export(export)
	case "ca":
		caOpts.Data = *data
		err = server.NewInstaller().CA(caOpts)
	case "admin-password":
		err = server.NewInstaller().AdminPassword(*data)
	}
	if err != nil {
		slog.Error(cmd, "err", err)
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
		fmt.Fprintln(os.Stderr, "kaze-syncd:", problem)
	}
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}
