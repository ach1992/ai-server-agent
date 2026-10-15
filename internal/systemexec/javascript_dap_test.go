package systemexec

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedJavaScriptDAPBundleProvenance(t *testing.T) {
	if node, adapter, err := JavaScriptDAP(); err == nil {
		if node == "" || adapter != JavaScriptDAPCLI {
			t.Fatalf("trusted JS DAP resource discovery selected an unexpected endpoint: %q %q", node, adapter)
		}
	}
	proof := os.Getenv("AGENT_JS_DAP_PROOF_BUNDLE")
	if proof == "" {
		t.Skip("optional verified upstream standalone DAP artifact missing")
	}
	if err := verifyJavaScriptDAPResources(proof, false); err != nil {
		t.Fatalf("upstream Microsoft js-debug v1.140.0 full pinned tree: %v", err)
	}
	if TrustedStaticFile(filepath.Join(proof, "src/dapDebugServer.js")) {
		t.Fatal("unprivileged disposable package acquired production adapter trust")
	}
}

func TestPinnedJavaScriptDAPRejectsMissingSymlinkAndChangedFiles(t *testing.T) {
	proof := os.Getenv("AGENT_JS_DAP_PROOF_BUNDLE")
	if proof == "" {
		t.Skip("optional upstream js-debug bundle fixture not provided")
	}
	if err := verifyJavaScriptDAPResources(t.TempDir(), false); err == nil {
		t.Fatal("missing transitive adapter resources were accepted")
	}
	root := t.TempDir()
	err := filepath.WalkDir(proof, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(proof, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.Mkdir(dst, 0700)
		}
		if !d.Type().IsRegular() {
			return os.ErrInvalid
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if _, err = io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyJavaScriptDAPResources(root, false); err != nil {
		t.Fatalf("copied exact bundle was not recognized: %v", err)
	}
	entry := filepath.Join(root, "src/dapDebugServer.js")
	original, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), original...)
	mutated[0] ^= 1
	if err := os.WriteFile(entry, mutated, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyJavaScriptDAPResources(root, false); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("single-byte dependency tampering accepted: %v", err)
	}
	if err := os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(proof, "src/dapDebugServer.js"), entry); err != nil {
		t.Fatal(err)
	}
	if err := verifyJavaScriptDAPResources(root, false); err == nil {
		t.Fatal("worker-supplied symlinked adapter allowed")
	}
}
