package systemexec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

const (
	TypeScript7Version = "7.0.2"
	// This operator-owned optional path is never created by an LSP request.
	TypeScript7Library  = "/usr/local/lib/ai-server-agent/lsp/typescript/7.0.2/lib"
	typeScript7Files    = 110
	typeScript7MaxBytes = 36 << 20
)

// Canonical SHA-256 over sorted lib/ entries: name NUL decimal byte length
// NUL raw bytes. The values were generated from @typescript/typescript-linux-*
// 7.0.2 npm published tarballs, with these npm integrity proofs:
// amd64  sha512-EYdf2cNg7rgCWJnxCdJ+F3V39O8ihb37eHAu1LK8oAFizgTQbPOK7zHHXbPt8rX24COqODXeI3sIf0fCXG7H/A==
// arm64  sha512-Qh4eU4/y3yDjnfjjyPYihMj5/ODIlmt+Bzu17OI+fiSRDW57QmU5SiN63exPRNJPKUzcc1INa1NXdrJ+MqHjUQ==
// Validating the entire lib directory, not only the native executable,
// prevents worker-controlled language-server resources from gaining trust.
var typeScript7PinnedTree = map[string]string{
	"amd64": "1803b0e2f76cee63a45e1762187c708b1b8b9c07307d1d3212651701e6463ed4",
	"arm64": "4b2f8c63e4059780792eecccc1ce44dc739ce0ae6c9afcc40af8eb196f6ad834",
}

func TypeScript7() (string, error) {
	if runtime.GOOS != "linux" || typeScript7PinnedTree[runtime.GOARCH] == "" {
		return "", errors.New("typescript7_unsupported_platform")
	}
	binary := filepath.Join(TypeScript7Library, "tsc")
	if !Trusted(binary) {
		return "", errors.New("typescript7_untrusted_executable")
	}
	if err := verifyTypeScript7Tree(TypeScript7Library, true, typeScript7PinnedTree[runtime.GOARCH]); err != nil {
		return "", fmt.Errorf("typescript7_integrity_or_owner_invalid: %w", err)
	}
	return binary, nil
}

func verifyTypeScript7Tree(dir string, rootOwned bool, expected string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("typescript7_missing_resource_directory")
	}
	if len(entries) != typeScript7Files {
		return errors.New("typescript7_unexpected_resource_count")
	}
	digest := sha256.New()
	var total int64
	haveBinary, haveLib := false, false
	for _, entry := range entries {
		name := entry.Name()
		// The directory is bounded and flat. Never hash a symlink target,
		// nested directory, device or other surprising external resource.
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
			return errors.New("typescript7_invalid_resource_name")
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("typescript7_invalid_resource_file")
		}
		if info.Size() < 0 || info.Size() > typeScript7MaxBytes || total > typeScript7MaxBytes-info.Size() {
			return errors.New("typescript7_resource_limit")
		}
		if rootOwned {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || st.Uid != 0 || info.Mode().Perm()&0022 != 0 {
				return errors.New("typescript7_untrusted_resource_owner")
			}
		}
		if name == "tsc" {
			haveBinary = info.Mode().Perm()&0111 != 0
		}
		if name == "lib.d.ts" {
			haveLib = true
		}
		_, _ = io.WriteString(digest, name)
		_, _ = digest.Write([]byte{0})
		_, _ = io.WriteString(digest, strconv.FormatInt(info.Size(), 10))
		_, _ = digest.Write([]byte{0})
		stream, err := os.Open(path)
		if err != nil {
			return errors.New("typescript7_resource_unreadable")
		}
		bytesRead, copyErr := io.Copy(digest, io.LimitReader(stream, info.Size()+1))
		closeErr := stream.Close()
		if copyErr != nil || closeErr != nil || bytesRead != info.Size() {
			return errors.New("typescript7_resource_changed_during_validation")
		}
		total += bytesRead
	}
	if !haveBinary || !haveLib || expected == "" || hex.EncodeToString(digest.Sum(nil)) != expected {
		return errors.New("typescript7_resource_digest_mismatch")
	}
	return nil
}
