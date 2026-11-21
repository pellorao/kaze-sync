// Command kaze-sync-setup is the Windows client installer: a console program whose own file carries the client
// binaries as a payload (design 9.5). Release tools append the payload and the manifest; the Web UI download
// appends the registration block. Run with no arguments it installs from itself.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"

	"kaze-sync/internal/client"
)

func main() {
	err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kaze-sync-setup:", err)
	}
	if runtime.GOOS == "windows" && interactive() { // a double-clicked window would close before it can be read
		fmt.Print("Press Enter to close this window.")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if err != nil {
		os.Exit(1)
	}
}

func run() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	o, err := client.ParseInstallArgs(os.Args[1:], self)
	if err != nil {
		return err
	}
	o.Out = os.Stdout
	if interactive() {
		o.Prompt = prompt
	}
	return client.Install(context.Background(), o)
}

func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// prompt asks for the registration line of the Web UI's "Add device" dialog; an empty line skips it.
func prompt() (server, code, fingerprint string, ok bool) {
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Paste the registration command from the Web UI (the line that starts with kaze-syncc enroll), or press Enter to skip: ")
		line, err := in.ReadString('\n')
		if len(line) <= 1 && err != nil || line == "\n" || line == "\r\n" {
			return "", "", "", false
		}
		if server, code, fingerprint, perr := client.ParseEnrollCommand(line); perr == nil {
			return server, code, fingerprint, true
		} else {
			fmt.Println(perr)
		}
		if err != nil {
			return "", "", "", false
		}
	}
}
