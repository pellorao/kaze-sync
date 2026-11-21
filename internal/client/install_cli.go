package client

import (
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
)

// ParseInstallArgs parses the arguments of `kaze-syncc install`: [--from FILE] [--dir DIR] [--force]
// [SERVER CODE [--fingerprint F]]. A program that is itself the installer passes its own path as defaultFrom.
// Flags may follow the positional arguments, as with enroll.
func ParseInstallArgs(args []string, defaultFrom string) (InstallOptions, error) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := InstallOptions{From: defaultFrom}
	fs.StringVar(&o.From, "from", defaultFrom, "installer file to install from")
	fs.StringVar(&o.Dir, "dir", "", "target directory")
	fs.BoolVar(&o.Force, "force", false, "install although the server is older than this package")
	fs.StringVar(&o.Fingerprint, "fingerprint", "", "pinned certificate or CA fingerprint")
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	switch len(pos) {
	case 0:
		if o.Fingerprint != "" {
			return o, errors.New("--fingerprint needs SERVER and CODE")
		}
	case 2:
		o.Server, o.Code = pos[0], pos[1]
	default:
		return o, errors.New("install takes either no registration or both SERVER and CODE")
	}
	return o, nil
}

// ParseEnrollCommand takes the line the Web UI shows for manual registration (`kaze-syncc enroll SERVER CODE
// [--fingerprint F]`, with or without the program path and `.exe`) as pasted by the user.
func ParseEnrollCommand(line string) (server, code, fingerprint string, err error) {
	var words []string
	for _, w := range strings.Fields(line) {
		words = append(words, strings.Trim(w, `"'`))
	}
	i := slices.Index(words, "enroll")
	if i < 0 {
		return "", "", "", errors.New(`paste the whole line that starts with "kaze-syncc enroll"`)
	}
	words = words[i+1:]
	var pos []string
	for i := 0; i < len(words); i++ {
		switch {
		case words[i] == "--fingerprint" && i+1 < len(words):
			fingerprint = words[i+1]
			i++
		case strings.HasPrefix(words[i], "--fingerprint="):
			fingerprint = strings.TrimPrefix(words[i], "--fingerprint=")
		case strings.HasPrefix(words[i], "-"):
			return "", "", "", errors.New("unknown option " + words[i])
		default:
			pos = append(pos, words[i])
		}
	}
	if len(pos) != 2 {
		return "", "", "", errors.New("the command needs a server address and a code")
	}
	return pos[0], pos[1], fingerprint, nil
}
