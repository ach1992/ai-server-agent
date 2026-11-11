package systemexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptionalAdapterTreeInspectionAndTamperRejection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node_modules")
	if err := os.MkdirAll(filepath.Join(root, "pkg", "src"), 0755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(root, "pkg", "src", "cli.js")
	if err := os.WriteFile(a, []byte("console.log(1)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	inspected, err := InspectResourceTree(root, 1024*1024)
	if err != nil || inspected.Files != 1 || inspected.Dirs != 2 || inspected.Bytes <= 0 {
		t.Fatalf("incomplete inspected resource: %+v %v", inspected, err)
	}
	p := nodeModulePolicy{label: "optional_adapter_test", fileCount: inspected.Files, dirCount: inspected.Dirs, maxBytes: 1024 * 1024, digest: inspected.SHA256}
	if err := verifyPinnedNodeModules(root, false, p); err != nil {
		t.Fatalf("valid tree rejected: %v", err)
	}
	if err := os.WriteFile(a, []byte("console.log(2)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedNodeModules(root, false, p); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered tree accepted: %v", err)
	}
	if err := os.Symlink(a, filepath.Join(root, "pkg", "badlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectResourceTree(root, 1024*1024); err == nil || !strings.Contains(err.Error(), "unsafe_dependency_type") {
		t.Fatalf("linked dependency accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "pkg", "badlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectResourceTree(root, 1024*1024); err == nil || !strings.Contains(err.Error(), "unwanted_package_bin") {
		t.Fatalf("generated wrapper directory accepted: %v", err)
	}
}

func TestOptionalAdapterProfileRejectsUntrustedAndMissingEntries(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "adapter.js")
	if err := os.WriteFile(file, []byte("module.exports=1"), 0644); err != nil {
		t.Fatal(err)
	}
	p := OptionalAdapterProfile{SchemaVersion: 1, Adapter: "sql-lsp", Version: "900.1.2+future",
		Artifacts:   []OptionalAdapterArtifact{{Path: file, Kind: "file", SHA256: strings.Repeat("a", 64), MaxBytes: 1000}},
		Entrypoints: map[string]string{"node": file, "cli": file}}
	if err := ValidateOptionalAdapterProfile("sql-lsp", p); err == nil {
		t.Fatal("worker-controlled adapter was trusted")
	}
	if err := os.Chmod(file, 0777); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOptionalAdapterProfile("sql-lsp", p); err == nil {
		t.Fatal("writable adapter accepted")
	}
	delete(p.Entrypoints, "cli")
	if err := ValidateOptionalAdapterProfile("sql-lsp", p); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("missing role accepted: %v", err)
	}
	if !validAdapterVersion("900.1.2+future") || validAdapterVersion("../../worker") {
		t.Fatal("version convention is too restrictive or allows paths")
	}
	badModule := OptionalAdapterProfile{SchemaVersion: 1, Adapter: "python-dap", Version: "9.9.9", Artifacts: []OptionalAdapterArtifact{{Path: dir, Kind: "tree", SHA256: strings.Repeat("a", 64), FileCount: 1, DirCount: 0, MaxBytes: 1024}}, Entrypoints: map[string]string{"modules": dir, "adapter": file}}
	if err := validateOptionalAdapterFields("python-dap", badModule); err == nil || !strings.Contains(err.Error(), "entry_mismatch") {
		t.Fatalf("profile claims an ignored Python adapter entrypoint: %v", err)
	}
	badManifest := filepath.Join(dir, "candidate.json")
	if err := os.WriteFile(badManifest, []byte(`{"schema_version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, installed, err := ReadOptionalAdapterProfile("sql-lsp", badManifest); !installed || err == nil {
		t.Fatalf("untrusted profile accepted or mistaken for absent: installed=%v err=%v", installed, err)
	}
}

func TestOptionalAdapterActivationRejectsUntrustedInputBeforeRootCopy(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "candidate.json")
	if err := os.WriteFile(candidate, []byte(`{"schema_version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageOptionalAdapterProfile("sql-lsp", candidate); err == nil || !strings.Contains(err.Error(), "root-owned") {
		t.Fatalf("worker candidate reached privileged staging without trust: %v", err)
	}
}

func TestOptionalAdapterProfileCLIInspectAndUnprivilegedActivation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunOptionalAdapter([]string{"inspect-tree", dir, "4096"}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect failed: %s", stderr.String())
	}
	var out ResourceTreeInspection
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Files != 1 || out.Dirs != 0 || len(out.SHA256) != 64 {
		t.Fatalf("inspection output invalid: %+v %v", out, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunOptionalAdapter([]string{"verify", "sql-lsp", filepath.Join(dir, "nonexistent")}, &stdout, &stderr); code == 0 {
		t.Fatal("missing manifest reported verified")
	}
	if os.Geteuid() != 0 {
		stdout.Reset()
		stderr.Reset()
		if code := RunOptionalAdapter([]string{"activate", "sql-lsp", filepath.Join(dir, "missing")}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "root") {
			t.Fatalf("unprivileged profile activation unexpectedly allowed: %s", stderr.String())
		}
	}
}

// Optional root-provisioning acceptance. CI must not assume any optional
// packages are installed; the explicit Dev candidate directory is tested only
// when provisioned by the operator in a nonproduction environment.
func TestOptionalAdapterRootProvisionedCandidateProfiles(t *testing.T) {
	directory := os.Getenv("AGENT_OPTIONAL_ADAPTER_CANDIDATE_DIR")
	if directory == "" {
		t.Skip("root-owned candidate fixtures not provisioned")
	}
	for name := range optionalAdapterRoles {
		t.Run(name, func(t *testing.T) {
			candidate := filepath.Join(directory, name+".json")
			if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
				t.Skip("this optional adapter has not been provisioned on this host")
			}
			p, present, err := ReadOptionalAdapterProfile(name, candidate)
			if err != nil || !present || p.Version == "" {
				t.Fatalf("candidate rejected: %+v %v %v", p, present, err)
			}
			if name == "php-dap" {
				if err := compatiblePHPXdebug(p.Entrypoints["php"], p.Entrypoints["xdebug"]); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func expectedOptionalTestEntry(t *testing.T, name, role, legacy string) string {
	t.Helper()
	profile, configured, err := ActiveOptionalAdapterProfile(name)
	if err != nil {
		t.Fatalf("invalid active %s profile: %v", name, err)
	}
	if configured {
		return profile.Entrypoints[role]
	}
	return legacy
}

// Two separately verified official debugpy wheels demonstrate that the
// administrator can select a different compatible upstream version without
// patching Agent constants or the entire Go runtime. This opt-in test uses
// root-owned fixtures only; absent CI fixtures do not imply readiness.
func TestOptionalAdapterAlternateDebugpyVersion(t *testing.T) {
	directory := os.Getenv("AGENT_OPTIONAL_ADAPTER_CANDIDATE_DIR")
	if directory == "" {
		t.Skip("optional root-owned second Wheel not installed")
	}
	current, present, err := ReadOptionalAdapterProfile("python-dap", filepath.Join(directory, "python-dap.json"))
	if err != nil || !present {
		t.Fatalf("primary wheel rejected: %v", err)
	}
	alternate, present, err := ReadOptionalAdapterProfile("python-dap", filepath.Join(directory, "python-dap-1.8.21.json"))
	if err != nil || !present {
		t.Fatalf("alternate wheel rejected: %v", err)
	}
	if current.Version == alternate.Version || current.Artifacts[0].SHA256 == alternate.Artifacts[0].SHA256 {
		t.Fatal("same resource bytes cannot prove independent version selectability")
	}
}
