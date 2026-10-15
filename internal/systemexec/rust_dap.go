package systemexec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Rust LLDB DAP is an OPTIONAL platform-managed runtime. The only admitted
// packaging identity is the exact Debian 13 LLVM 19.1.7-3+b1 distro snapshot
// with immutable executable/library digests from its official .deb artifacts.
// This is not a fallback to an unpinned PATH executable, not an Agent
// installer, and never supplies a project-controlled LD_LIBRARY_PATH.
const RustDAPVersion = "19.1.7"
const RustDAPBinary = "/usr/lib/llvm-19/bin/lldb-dap"

type rustDapNativeArtifact struct {
	file       string
	digest     string
	min, max   int64
	executable bool
}

func rustDAPArtifacts(arch string) []rustDapNativeArtifact {
	libdir := "/usr/lib/x86_64-linux-gnu"
	if arch == "arm64" {
		libdir = "/usr/lib/aarch64-linux-gnu"
	}
	base := "/usr/lib/llvm-19"
	artifacts := map[string][]rustDapNativeArtifact{
		"amd64": {
			{RustDAPBinary, "206489a92fe8c1cbaefe9db970dccb54add9030cf84c6b744425c7c7e0bc2782", 300 << 10, 1 << 20, true},
			{filepath.Join(base, "bin/lldb-server"), "70f01ed201af0ab9c81269da5ac0d9703e2c633d8ba91fce5162de98817f0b59", 1 << 20, 8 << 20, true},
			{filepath.Join(base, "bin/lldb-argdumper"), "34127b4586081551d38196d4a0404d91806f4ee9fa470220272d22bbe390b5fc", 8 << 10, 100 << 10, true},
			{filepath.Join(base, "lib/liblldbIntelFeatures.so.19.1"), "7cdfffaa9889bb05d0f05eacc432fee0f86902d8bd47828c9ef50e5c09725ff3", 8 << 10, 100 << 10, false},
			{filepath.Join(libdir, "liblldb-19.so.1"), "f0978d51d37ca84ba2cd6f1e38c0c35f64dbb3fc8d3c5326cbf00333b40d0690", 1 << 20, 65 << 20, false},
			{filepath.Join(libdir, "libclang-cpp.so.19.1"), "0108c687d2f500d77687984c46c9fad663782603f5c623e3c14224c155b4fd21", 1 << 20, 140 << 20, false},
		},
		"arm64": {
			{RustDAPBinary, "a163a65fe9aa989ba9f25f9767365879655945f8c46d8fb1b24e7d667b04f633", 300 << 10, 1 << 20, true},
			{filepath.Join(base, "bin/lldb-server"), "3bc804f3ef077ed57b5f9df223ed4440bd93069203fe4967c8d8daf3824ade5e", 1 << 20, 8 << 20, true},
			{filepath.Join(base, "bin/lldb-argdumper"), "f058880729df806c9aa017079bc4ea4be0d8886edacaec8e187993e96e67ee9f", 8 << 10, 100 << 10, true},
			{filepath.Join(base, "lib/liblldbIntelFeatures.so.19.1"), "ee773e97669b850ca14974de07de0ff1c7b3f774ba5722ed757d68ffb14c4a7a", 8 << 10, 100 << 10, false},
			{filepath.Join(libdir, "liblldb-19.so.1"), "b575e56ecb06d436d3d8d0d3abf8542a694f2984a0bd309454d1263017462613", 1 << 20, 65 << 20, false},
			{filepath.Join(libdir, "libclang-cpp.so.19.1"), "e154e7721ef2df684630d1747e9bec85ab83907e961a7c51f59e4b4551b222fa", 1 << 20, 140 << 20, false},
		},
	}
	return artifacts[arch]
}

// These hashes are for the official Debian 13 build, not a promise that
// coincidentally identical binaries are safe on a different native runtime.
func rustDapDebian13(osRelease string) bool {
	var id, version string
	for _, line := range strings.Split(osRelease, "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		value := strings.Trim(parts[1], `"'`)
		switch parts[0] {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	return id == "debian" && version == "13"
}

func RustLLDBDAP() (string, error) {
	artifacts := rustDAPArtifacts(runtime.GOARCH)
	if runtime.GOOS != "linux" || len(artifacts) == 0 {
		return "", errors.New("rust_dap_verified_distribution_unavailable")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil || !rustDapDebian13(string(release)) {
		return "", errors.New("rust_dap_unverified_distribution_version")
	}
	for _, artifact := range artifacts {
		trusted := TrustedStaticFile(artifact.file)
		if artifact.executable {
			trusted = Trusted(artifact.file)
		}
		if !trusted {
			return "", fmt.Errorf("rust_dap_untrusted_or_missing_native_artifact: %s", filepath.Base(artifact.file))
		}
		if err := verifyShellNativeBinary(artifact.file, artifact.digest, artifact.min, artifact.max); err != nil {
			return "", fmt.Errorf("rust_dap_native_integrity_invalid: %s: %w", filepath.Base(artifact.file), err)
		}
	}
	// Host OS/libc/LLVM runtime remains under distro-package trust; no
	// project-supplied loader or dependency paths are admitted. LLDB's
	// meaningful debuggee/cgroup readiness is verified separately by DAP.
	return RustDAPBinary, nil
}
