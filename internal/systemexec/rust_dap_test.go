package systemexec

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRustDAPNativePinnedProvenanceMatrix(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			artifacts := rustDAPArtifacts(arch)
			if len(artifacts) != 6 || !artifacts[0].executable || artifacts[0].file != RustDAPBinary {
				t.Fatalf("missing mandatory pinned LLDB adapter/helper/native library chain: %+v", artifacts)
			}
			seen := make(map[string]bool)
			for _, artifact := range artifacts {
				if artifact.digest == "" || len(artifact.digest) != 64 || artifact.min <= 0 || artifact.max <= artifact.min ||
					!filepath.IsAbs(artifact.file) || seen[artifact.file] || strings.Contains(artifact.file, "..") {
					t.Fatalf("incomplete/ambiguous LLDB supply-chain policy: %+v", artifact)
				}
				seen[artifact.file] = true
			}
		})
	}
	if got := rustDAPArtifacts("arm"); got != nil {
		t.Fatalf("unverified architecture got trusted LLDB profile: %+v", got)
	}
}

func TestRustDAPReadinessNeverProvisionsHost(t *testing.T) {
	// Passive readiness may be unavailable (especially on CI/test images).
	// An unavailable optional native adapter must not disable the core Agent.
	got, err := RustLLDBDAP()
	if err == nil && (runtime.GOOS != "linux" || got != RustDAPBinary) {
		t.Fatalf("trusted Rust adapter readiness returned unexpected path: %q", got)
	}
	if err != nil && got != "" {
		t.Fatalf("untrusted Rust profile exposed executable: %q %v", got, err)
	}
}

func TestRustDAPDistributionReleaseFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		source  string
		allowed bool
	}{
		{`NAME="Debian GNU/Linux"
ID=debian
VERSION_ID="13"`, true},
		{"ID=ubuntu\nVERSION_ID=24.04", false},
		{"ID=debian\nVERSION_ID=12", false},
		{"ID=debian\nVERSION_ID=13\nID=ubuntu", false},
		{"ID=debian\nVERSION_ID=13\nVERSION_ID=12", false},
		{"ID=debian-testing\nVERSION_ID=13", false},
	} {
		if got := rustDapDebian13(tc.source); got != tc.allowed {
			t.Errorf("unexpected distro trust: %q -> %t", tc.source, got)
		}
	}
}
