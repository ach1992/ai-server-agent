package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
	"golang.org/x/sys/unix"
)

// Local operator enrollment intentionally requires an interactive controlling
// terminal and never accepts secret bytes in command-line flags, environment
// variables, MCP parameters, shell history or normal stdout.
func enrollServiceCredential(cfg config.Config, argv []string) error {
	if os.Geteuid() != 0 {
		return errors.New("service-access enroll requires a root-owned interactive operator session")
	}
	if len(argv) < 1 || argv[0] != "enroll" {
		return errors.New("usage: ai-server-agent -config PATH service-access enroll --ref svc_ID [--kind password|token] [--username NAME] [--ttl-days 30 | --never]")
	}
	flags := flag.NewFlagSet("service-access enroll", flag.ContinueOnError)
	ref := flags.String("ref", "", "opaque reference previously created through service_access register")
	kind := flags.String("kind", "password", "password or token")
	username := flags.String("username", "", "non-secret login username (optional)")
	ttl := flags.Int("ttl-days", 0, "credential lifetime in days (default 30)")
	never := flags.Bool("never", false, "explicitly disable expiration")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(argv[1:]); err != nil {
		return errors.New("invalid enrollment flags; never supply a secret in command arguments")
	}
	if len(flags.Args()) != 0 {
		return errors.New("unexpected argument; never pass a password on the command line")
	}
	if *ref == "" {
		return errors.New("--ref is required")
	}
	if *never && *ttl != 0 {
		return errors.New("--ttl-days and --never cannot both be selected")
	}
	store := serviceaccess.New(cfg.StateDir)
	known, err := store.List()
	if err != nil {
		return errors.New("protected service store unavailable or invalid; inspect local permissions before retry")
	}
	found := false
	for _, record := range known {
		if record.ID == *ref && record.HostID == cfg.InstanceID && record.RevokedAt == nil {
			found = true
			break
		}
	}
	if !found {
		return errors.New("service reference not found, revoked or bound to another Agent host")
	}
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		return errors.New("interactive /dev/tty unavailable; use an authorized operator terminal to enroll")
	}
	tty := os.NewFile(uintptr(fd), "/dev/tty")
	defer tty.Close()
	term, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return errors.New("controlling terminal echo control unavailable")
	}
	noEcho := *term
	noEcho.Lflag &^= unix.ECHO | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return errors.New("cannot disable terminal echo")
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, term)
	fmt.Fprint(tty, "Enter target-service secret (hidden): ")
	reader := bufio.NewReaderSize(tty, 8192)
	line, err := reader.ReadSlice('\n')
	fmt.Fprint(tty, "\n")
	if err != nil {
		return errors.New("secret enrollment cancelled, input unavailable or too large")
	}
	defer func() {
		for i := range line {
			line[i] = 0
		}
	}()
	value := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
	if len(value) == 0 || len(value) > 4096 {
		return errors.New("secret must contain 1..4096 bytes")
	}
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, term); err != nil {
		return errors.New("failed to restore terminal echo; credential not saved")
	}
	meta, err := store.Enroll(*ref, *kind, *username, value, *ttl, *never)
	if err != nil {
		return err
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
