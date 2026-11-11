package systemexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Profiles are selected by an administrator, not by the worker, workspace,
// PATH, or an LSP request. Each profile pins the complete resources that can
// supply executable code. The profile is immutable to non-root callers and is
// reread on each discovery/launch: changing a compatible adapter version does
// not require rebuilding or restarting the Agent.
const OptionalAdapterProfilesDir = "/usr/local/lib/ai-server-agent/adapter-profiles"

var optionalAdapterName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

type OptionalAdapterArtifact struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"` // file or complete, symlink-free tree
	SHA256    string `json:"sha256"`
	FileCount int    `json:"file_count,omitempty"`
	DirCount  int    `json:"dir_count,omitempty"`
	MaxBytes  int64  `json:"max_bytes"`
}

type OptionalAdapterProfile struct {
	SchemaVersion int    `json:"schema_version"`
	Adapter       string `json:"adapter"`
	Version       string `json:"version"`
	// Source provenance is for the operator receipt. Code integrity is
	// enforced against exact extracted artifacts, not a URL at runtime.
	Source       string                    `json:"source,omitempty"`
	SourceSHA256 string                    `json:"source_sha256,omitempty"`
	Artifacts    []OptionalAdapterArtifact `json:"artifacts"`
	Entrypoints  map[string]string         `json:"entrypoints"`
}

// Roles are interface/protocol roles, not hardcoded tool versions. An optional
// profile may use a different compatible Node, LSP, DAP or native release.
var optionalAdapterRoles = map[string][]string{
	"bash-lsp":           {"node", "cli", "shellcheck"},
	"web-lsp":            {"node", "html", "css"},
	"sql-lsp":            {"node", "cli"},
	"python-dap":         {"modules", "adapter"},
	"javascript-dap":     {"node", "cli"},
	"php-lsp":            {"phar"},
	"python-lsp":         {"binary"},
	"typescript-lsp":     {"binary"},
	"rust-lsp":           {"analyzer", "rustc", "sysroot-source"},
	"rust-dap":           {"lldb", "server", "argdumper", "features", "liblldb", "libclang"},
	"javascript-browser": {"binary"},
	// PHP's debug ABI is separately checked by the PHP DAP integration.
	"php-dap": {"node", "cli", "php", "xdebug"},
}

func ActiveOptionalAdapterProfile(name string) (OptionalAdapterProfile, bool, error) {
	if _, ok := optionalAdapterRoles[name]; !ok || !optionalAdapterName.MatchString(name) {
		return OptionalAdapterProfile{}, false, errors.New("adapter_profile_unknown_adapter")
	}
	return ReadOptionalAdapterProfile(name, filepath.Join(OptionalAdapterProfilesDir, name+".json"))
}

// ReadOptionalAdapterProfile validates a root-owned candidate without installing
// or activating it. An absent *active* profile is a legitimate legacy fallback;
// an invalid or partial active profile is never silently ignored.
func ReadOptionalAdapterProfile(name, path string) (OptionalAdapterProfile, bool, error) {
	profile, found, err := ReadOptionalAdapterProfileMetadata(name, path)
	if err != nil || !found {
		return profile, found, err
	}
	if err := ValidateOptionalAdapterProfile(name, profile); err != nil {
		return OptionalAdapterProfile{}, true, err
	}
	return profile, true, nil
}

// ReadOptionalAdapterProfileMetadata is for descriptive readiness responses
// only. No executable selection uses it: execution always calls the complete
// provenance verifier. Avoid hashing hundreds of MB again just to print the
// selected version and paths after the parallel readiness checks.
func ReadOptionalAdapterProfileMetadata(name, path string) (OptionalAdapterProfile, bool, error) {
	if _, ok := optionalAdapterRoles[name]; !ok || !optionalAdapterName.MatchString(name) {
		return OptionalAdapterProfile{}, false, errors.New("adapter_profile_unknown_adapter")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return OptionalAdapterProfile{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 32<<10 || !TrustedStaticFile(path) {
		return OptionalAdapterProfile{}, true, errors.New("adapter_profile_untrusted_or_invalid_file")
	}
	f, err := os.Open(path)
	if err != nil {
		return OptionalAdapterProfile{}, true, errors.New("adapter_profile_unreadable")
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 32<<10+1))
	dec.DisallowUnknownFields()
	var p OptionalAdapterProfile
	if err := dec.Decode(&p); err != nil {
		return OptionalAdapterProfile{}, true, fmt.Errorf("adapter_profile_invalid_json: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return OptionalAdapterProfile{}, true, errors.New("adapter_profile_trailing_data")
	}
	if err := validateOptionalAdapterFields(name, p); err != nil {
		return OptionalAdapterProfile{}, true, err
	}
	return p, true, nil
}

func SelectedOptionalAdapterMetadata(name string) (OptionalAdapterProfile, bool, error) {
	if _, ok := optionalAdapterRoles[name]; !ok || !optionalAdapterName.MatchString(name) {
		return OptionalAdapterProfile{}, false, errors.New("adapter_profile_unknown_adapter")
	}
	return ReadOptionalAdapterProfileMetadata(name, filepath.Join(OptionalAdapterProfilesDir, name+".json"))
}

func validProfilePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func validDigest(d string) bool {
	if len(d) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(d)
	return err == nil && d == strings.ToLower(d)
}

func validAdapterVersion(v string) bool {
	if len(v) == 0 {
		return false
	}
	for _, c := range v {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '.' || c == '-' || c == '_' || c == '+') {
			return false
		}
	}
	return true
}

// ValidateOptionalAdapterProfile checks the exact active resource set. A
// profile cannot authorize an entrypoint that is not covered by an immutable
// file or a complete pinned resource tree, even if that path is root-owned.
func ValidateOptionalAdapterProfile(name string, p OptionalAdapterProfile) error {
	if err := validateOptionalAdapterFields(name, p); err != nil {
		return err
	}
	for _, a := range p.Artifacts {
		switch a.Kind {
		case "tree":
			if err := verifyPinnedNodeModules(a.Path, true, nodeModulePolicy{
				label: "adapter_profile", fileCount: a.FileCount, dirCount: a.DirCount,
				maxBytes: a.MaxBytes, digest: a.SHA256,
			}); err != nil {
				return fmt.Errorf("adapter tree %s: %w", a.Path, err)
			}
		case "file":
			if err := verifyProfileFile(a.Path, a.SHA256, a.MaxBytes); err != nil {
				return fmt.Errorf("adapter file %s: %w", a.Path, err)
			}
		}
	}
	for role, path := range p.Entrypoints {
		if role == "modules" || (name == "rust-lsp" && role == "sysroot-source") {
			info, err := os.Lstat(path)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("adapter_profile_directory_entry_invalid_%s", role)
			}
			if role == "modules" && !TrustedStaticFile(filepath.Join(path, "debugpy", "adapter", "__main__.py")) {
				return errors.New("adapter_profile_modules_root_invalid")
			}
			if role == "sysroot-source" && !TrustedStaticFile(filepath.Join(path, "core", "src", "lib.rs")) {
				return errors.New("adapter_profile_rust_sysroot_root_untrusted")
			}
			continue
		}
		if !TrustedStaticFile(path) {
			return fmt.Errorf("adapter_profile_untrusted_entry_%s", role)
		}
		if role == "node" || role == "binary" || role == "analyzer" || role == "rustc" || role == "php" || role == "shellcheck" || role == "lldb" || role == "server" || role == "argdumper" {
			if !Trusted(path) {
				return fmt.Errorf("adapter_profile_entry_not_executable_%s", role)
			}
		}
	}
	return nil
}

// Structural validation only: never authorize execution without separately
// verifying actual root-owned file/directory content via Validate above.
func validateOptionalAdapterFields(name string, p OptionalAdapterProfile) error {
	roles, ok := optionalAdapterRoles[name]
	if !ok || p.SchemaVersion != 1 || p.Adapter != name || !validAdapterVersion(p.Version) ||
		len(p.Artifacts) == 0 || len(p.Entrypoints) != len(roles) {
		return errors.New("adapter_profile_schema_or_roles_invalid")
	}
	if p.SourceSHA256 != "" && !validDigest(p.SourceSHA256) {
		return errors.New("adapter_profile_source_sha256_invalid")
	}
	for _, role := range roles {
		if !validProfilePath(p.Entrypoints[role]) {
			return fmt.Errorf("adapter_profile_missing_or_invalid_role_%s", role)
		}
	}
	// debugLaunchPython uses runpy.run_module("debugpy.adapter"). Do not
	// advertise a configurable entrypoint that the launcher would ignore.
	if name == "python-dap" && p.Entrypoints["adapter"] != filepath.Join(p.Entrypoints["modules"], "debugpy", "adapter", "__main__.py") {
		return errors.New("adapter_profile_python_module_entry_mismatch")
	}
	seen := make(map[string]bool, len(p.Artifacts))
	for _, a := range p.Artifacts {
		if !validProfilePath(a.Path) || !validDigest(a.SHA256) || a.MaxBytes <= 0 || seen[a.Path] {
			return errors.New("adapter_profile_artifact_fields_invalid")
		}
		seen[a.Path] = true
		switch a.Kind {
		case "tree":
			if a.FileCount <= 0 || a.DirCount < 0 {
				return errors.New("adapter_profile_tree_counts_invalid")
			}
		case "file":
			if a.FileCount != 0 || a.DirCount != 0 {
				return errors.New("adapter_profile_file_counts_invalid")
			}
		default:
			return errors.New("adapter_profile_artifact_kind_invalid")
		}
	}
	for _, role := range roles {
		path := p.Entrypoints[role]
		covered, isTreeRoot, treeCovered := false, false, false
		for _, a := range p.Artifacts {
			if a.Kind == "file" && path == a.Path {
				covered = true
			}
			if a.Kind == "tree" && (path == a.Path || strings.HasPrefix(path, a.Path+string(filepath.Separator))) {
				covered = true
				isTreeRoot = isTreeRoot || path == a.Path
				treeCovered = true
			}
		}
		if !covered {
			return fmt.Errorf("adapter_profile_unpinned_entry_%s", role)
		}
		if name == "rust-lsp" && role == "sysroot-source" && !isTreeRoot {
			return errors.New("adapter_profile_rust_sysroot_requires_complete_tree")
		}
		if name == "javascript-browser" && role == "binary" && !treeCovered {
			return errors.New("adapter_profile_browser_requires_complete_bundle")
		}
		if isTreeRoot && role != "modules" && !(name == "rust-lsp" && role == "sysroot-source") {
			return fmt.Errorf("adapter_profile_untrusted_entry_%s", role)
		}
	}
	return nil
}

func verifyProfileFile(path, want string, maxBytes int64) error {
	if !TrustedStaticFile(path) {
		return errors.New("adapter_profile_file_untrusted")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > maxBytes {
		return errors.New("adapter_profile_file_size_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("adapter_profile_file_unreadable")
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, io.LimitReader(f, st.Size()+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n != st.Size() {
		return errors.New("adapter_profile_file_changed_during_validation")
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return errors.New("adapter_profile_file_digest_mismatch")
	}
	return nil
}

func SelectedOptionalAdapter(name string) (paths map[string]string, version string, configured bool, err error) {
	profile, configured, err := ActiveOptionalAdapterProfile(name)
	if err != nil || !configured {
		return nil, "", configured, err
	}
	return profile.Entrypoints, profile.Version, true, nil
}

// Operator-facing informational label. Only active, verified resource profiles
// provide a reported selected version; the legacy constants remain fallback.
func OptionalAdapterVersion(name, legacy string) string {
	profile, configured, err := SelectedOptionalAdapterMetadata(name)
	if configured {
		if err != nil {
			return ""
		}
		return profile.Version
	}
	return legacy
}
