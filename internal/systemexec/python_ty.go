package systemexec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
)

const (
	PythonTyVersion = "0.0.87"
	PythonTyBinary  = "/usr/local/lib/ai-server-agent/lsp/python/ty/0.0.87/ty"
)

// Python ty 0.0.87 is a standalone Rust native binary, not an interpreter
// loading worker-owned Python packages. These exact SHA-256 fingerprints were
// read from the native binary member of the PyPI manylinux2014 wheels:
// amd64 wheel sha256 e8caa3316ea81e794fcd98d1e4d3aaa5fae84790154d88c3e85ac3bd01c537bd
// arm64 wheel sha256 a6ed0a498db9fa8ffb421014698fbca6fa32699d9544156aeeda763b8d8984d0
var pythonTyBinarySha = map[string]string{
	"amd64": "d90608cd5ccdc786ead2bc0cbf2268dfd6008286f0bd010259221fabe1479658",
	"arm64": "d09308847363bd05ea7df388ae5afbf657bfb03fbc94966c58525ae5fde14cd5",
}

func TyPython() (string, error) {
	expected := pythonTyBinarySha[runtime.GOARCH]
	if runtime.GOOS != "linux" || expected == "" {
		return "", errors.New("python_ty_unsupported_platform")
	}
	if !Trusted(PythonTyBinary) {
		return "", errors.New("python_ty_untrusted_executable")
	}
	if err := verifyTyPythonBinary(PythonTyBinary, expected); err != nil {
		return "", err
	}
	return PythonTyBinary, nil
}

func verifyTyPythonBinary(binary, expected string) error {
	stat, err := os.Lstat(binary)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 20<<20 || stat.Size() > 34<<20 {
		return errors.New("python_ty_binary_invalid")
	}
	f, err := os.Open(binary)
	if err != nil {
		return errors.New("python_ty_binary_unreadable")
	}
	sum := sha256.New()
	n, readErr := io.Copy(sum, io.LimitReader(f, stat.Size()+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || n != stat.Size() {
		return errors.New("python_ty_binary_changed_during_validation")
	}
	if hex.EncodeToString(sum.Sum(nil)) != expected || expected == "" {
		return errors.New("python_ty_binary_digest_mismatch")
	}
	return nil
}
