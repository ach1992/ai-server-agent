package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
	"golang.org/x/sys/unix"
)

type serviceCredentialFlags struct {
	operation  string
	ref        string
	service    string
	endpoint   string
	purpose    string
	kind       string
	username   string
	secretFile string
	ttlDays    int
	never      bool
}

func parseServiceCredentialFlags(argv []string) (serviceCredentialFlags, error) {
	if len(argv) < 1 || (argv[0] != "enroll" && argv[0] != "add") {
		return serviceCredentialFlags{}, errors.New("usage: service-access add --service NAME --endpoint URI [--kind KIND] [--secret-file ROOT_ONLY_FILE] | service-access enroll --ref svc_ID [--kind KIND] [--secret-file ROOT_ONLY_FILE]")
	}
	mode := argv[0]
	flags := flag.NewFlagSet("service-access "+mode, flag.ContinueOnError)
	ref := flags.String("ref", "", "existing opaque service reference")
	service := flags.String("service", "", "nonsecret service name (add only)")
	endpoint := flags.String("endpoint", "", "nonsecret service target URI (add only)")
	purpose := flags.String("purpose", "", "nonsecret purpose label (add only)")
	kind := flags.String("kind", "password", "nonsecret credential kind, e.g. password, api_key, private_key, access_code")
	username := flags.String("username", "", "nonsecret username (optional)")
	secretFile := flags.String("secret-file", "", "optional root-owned 0400/0600 regular text file, no symlinks; exact contents")
	ttlDays := flags.Int("ttl-days", 0, "credential lifetime in days (default 30)")
	never := flags.Bool("never", false, "explicitly disable expiration")
	flags.SetOutput(io.Discard)
	if err := flags.Parse(argv[1:]); err != nil || len(flags.Args()) != 0 {
		return serviceCredentialFlags{}, errors.New("invalid service-access options; never pass the secret itself as a command argument")
	}
	if !serviceaccess.ValidKind(*kind) || *ttlDays < 0 || *ttlDays > 36500 || (*never && *ttlDays != 0) {
		return serviceCredentialFlags{}, errors.New("invalid credential kind or expiry options")
	}
	if mode == "enroll" {
		if *ref == "" || *service != "" || *endpoint != "" || *purpose != "" {
			return serviceCredentialFlags{}, errors.New("enroll requires --ref; service identity changes require an explicit separate update")
		}
	} else {
		if *ref != "" || *service == "" || *endpoint == "" {
			return serviceCredentialFlags{}, errors.New("add requires --service and --endpoint; --ref is only for enroll")
		}
		if err := serviceaccess.ValidServiceTarget(*endpoint); err != nil {
			return serviceCredentialFlags{}, errors.New("invalid service target URI: no secrets, userinfo, query, or fragment")
		}
	}
	return serviceCredentialFlags{
		operation: mode, ref: *ref, service: *service, endpoint: *endpoint,
		purpose: *purpose, kind: *kind, username: *username, secretFile: *secretFile,
		ttlDays: *ttlDays, never: *never,
	}, nil
}

// readRootOwnedSecretFile explicitly checks the complete absolute pathname,
// not only its leaf, to reject caller-controlled symlink/path replacements.
// A secret file is a deliberate operator-owned protected input, never a
// worker artifact and never an MCP client argument containing secret bytes.
func readRootOwnedSecretFile(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("secret-file must be an absolute canonical pathname")
	}
	for current := path; ; current = filepath.Dir(current) {
		fi, err := os.Lstat(current)
		if err != nil {
			return "", errors.New("secret-file path unavailable")
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0022 != 0 {
			return "", errors.New("secret-file path is not root-owned or is writable by others")
		}
		if current == path {
			if !fi.Mode().IsRegular() || st.Nlink != 1 || (fi.Mode().Perm() != 0400 && fi.Mode().Perm() != 0600) {
				return "", errors.New("secret-file must be a root-only regular file without hardlinks")
			}
		} else if !fi.IsDir() {
			return "", errors.New("secret-file parent must be a trusted directory")
		}
		if current == "/" {
			break
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", errors.New("secret-file cannot be opened safely")
	}
	file := os.NewFile(uintptr(fd), "protected-service-secret")
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return "", errors.New("secret-file metadata unavailable")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != 0 || identity.Nlink != 1 || !stat.Mode().IsRegular() ||
		(stat.Mode().Perm() != 0400 && stat.Mode().Perm() != 0600) {
		return "", errors.New("secret-file identity changed or is insufficiently protected")
	}
	content, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(content) == 0 || len(content) > 4096 {
		return "", errors.New("secret-file must contain 1..4096 bytes of text")
	}
	// Do not trim or transform PEM newlines, whitespace or token bytes.
	return string(content), nil
}

// The TTY prompt accepts one-line secrets without echo. For multi-line PEM,
// certificates or long keys, use --secret-file with protected local staging.
func readServiceSecretFromTTY() (string, error) {
	fd, err := unix.Open("/dev/tty", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		return "", errors.New("interactive /dev/tty unavailable; use an authorized operator terminal or --secret-file")
	}
	tty := os.NewFile(uintptr(fd), "/dev/tty")
	defer tty.Close()
	term, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", errors.New("controlling terminal echo control unavailable")
	}
	noEcho := *term
	noEcho.Lflag &^= unix.ECHO | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", errors.New("cannot disable terminal echo")
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, term)
	fmt.Fprint(tty, "Enter target-service credential (hidden): ")
	reader := bufio.NewReaderSize(tty, 4097)
	line, err := reader.ReadSlice('\n')
	fmt.Fprint(tty, "\n")
	if err != nil {
		return "", errors.New("credential input cancelled, unavailable or too large")
	}
	defer func() {
		for i := range line {
			line[i] = 0
		}
	}()
	value := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
	if len(value) == 0 || len(value) > 4096 {
		return "", errors.New("credential must contain 1..4096 bytes")
	}
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, term); err != nil {
		return "", errors.New("failed to restore terminal echo; credential not saved")
	}
	return value, nil
}

// Local operator enrollment and creation are deliberately root-only.
// A single "add" command creates the metadata and secret atomically;
// "enroll" retains the existing ref and is suitable for rotation.
func enrollServiceCredential(cfg config.Config, argv []string) error {
	if os.Geteuid() != 0 {
		return errors.New("service-access requires a root-owned operator session")
	}
	options, err := parseServiceCredentialFlags(argv)
	if err != nil {
		return err
	}
	store := serviceaccess.New(cfg.StateDir)
	if options.operation == "enroll" {
		known, err := store.List()
		if err != nil {
			return errors.New("protected service store unavailable or invalid; inspect local permissions")
		}
		found := false
		for _, record := range known {
			if record.ID == options.ref && record.HostID == cfg.InstanceID && record.RevokedAt == nil {
				found = true
				break
			}
		}
		if !found {
			return errors.New("service reference not found, revoked or bound to another Agent host")
		}
	}
	// Validate newly created registration before requesting its secret.
	if options.operation == "add" && cfg.InstanceID == "" {
		return errors.New("configured Agent instance_id required")
	}
	var value string
	if options.secretFile != "" {
		value, err = readRootOwnedSecretFile(options.secretFile)
	} else {
		value, err = readServiceSecretFromTTY()
	}
	if err != nil {
		return err
	}
	var meta serviceaccess.Metadata
	if options.operation == "add" {
		meta, err = store.RegisterWithSecret(serviceaccess.Registration{
			HostID: cfg.InstanceID, Service: options.service, Purpose: options.purpose,
			Endpoint: options.endpoint, Username: options.username,
		}, options.kind, value, options.ttlDays, options.never)
	} else {
		meta, err = store.Enroll(options.ref, options.kind, options.username, value,
			options.ttlDays, options.never)
	}
	if err != nil {
		return err
	}
	result, err := json.Marshal(meta)
	if err != nil {
		return errors.New("credential saved but metadata encoding unavailable; use service_access inspect")
	}
	fmt.Println(string(result))
	return nil
}
