package executor

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/audit"
	"github.com/ach1992/ai-server-agent/internal/policy"
	"golang.org/x/sys/unix"
)

func newFileTestServer(t *testing.T, protected []string) *Server {
	t.Helper()
	return &Server{
		guard: policy.New(protected),
		audit: audit.New(filepath.Join(t.TempDir(), "audit.jsonl")),
	}
}

func TestReadFileRangedUTF8AndVersion(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("abcdefghij"), 0600); err != nil {
		t.Fatal(err)
	}

	first := server.readFile(Request{Path: path, Offset: 0, Limit: 4})
	if !first.OK || first.Output != "abcd" || first.OutputEncoding != "utf-8" {
		t.Fatalf("first range = %+v", first)
	}
	if first.RequestedOffset != 0 || first.NextOffset != 4 || first.FileSize != 10 || first.EOF || !first.Truncated || first.OmittedBytes != 6 {
		t.Fatalf("first metadata = %+v", first)
	}
	if first.FileVersion == "" {
		t.Fatal("missing file version")
	}

	second := server.readFile(Request{Path: path, Offset: first.NextOffset, Limit: 32, FileVersion: first.FileVersion})
	if !second.OK || second.Output != "efghij" || second.NextOffset != 10 || !second.EOF || second.Truncated {
		t.Fatalf("second range = %+v", second)
	}
	if second.FileVersion != first.FileVersion {
		t.Fatalf("file version drifted across unchanged ranges: %q != %q", second.FileVersion, first.FileVersion)
	}
}

func TestReadFileBinaryUsesBase64(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "binary.bin")
	raw := []byte{0xff, 0x00, 0x81, 0x7f}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	resp := server.readFile(Request{Path: path, Limit: len(raw)})
	if !resp.OK || resp.OutputEncoding != "base64" || resp.Output != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("binary response = %+v", resp)
	}
	if !resp.EOF || resp.BytesReturned != int64(len(raw)) {
		t.Fatalf("binary metadata = %+v", resp)
	}
}

func TestReadFileConsistencyTokenDetectsChange(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "versioned.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	first := server.readFile(Request{Path: path, Limit: 16})
	if !first.OK {
		t.Fatalf("initial read = %+v", first)
	}
	if err := os.WriteFile(path, []byte("after-change"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := server.readFile(Request{Path: path, Limit: 16, FileVersion: first.FileVersion})
	if changed.OK || changed.ErrorCode != "file_changed" {
		t.Fatalf("stale token response = %+v", changed)
	}
}

func TestReadFileProtectedSymlinkAliasRequiresApproval(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "protected-ai-server-agent")
	if err := os.Mkdir(protectedDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(protectedDir, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.txt")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	server := newFileTestServer(t, []string{protectedDir})

	blocked := server.readFile(Request{Path: alias, Limit: 32})
	if blocked.OK || blocked.ErrorCode != "approval_required" {
		t.Fatalf("protected alias was not blocked: %+v", blocked)
	}
	approved := server.readFile(Request{Path: alias, Limit: 32, Approval: true})
	if !approved.OK || approved.Output != "secret" {
		t.Fatalf("approved protected alias read = %+v", approved)
	}
}

func TestReadFileRejectsSpecialFile(t *testing.T) {
	server := newFileTestServer(t, nil)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	resp := server.readFile(Request{Path: fifo, Limit: 16})
	if resp.OK || resp.ErrorCode != "unsafe_file_type" {
		t.Fatalf("FIFO read response = %+v", resp)
	}
}

func TestReadFileBoundsLargeRegularFile(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "large.txt")
	content := strings.Repeat("x", maxFileReadBytes+4096)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	resp := server.readFile(Request{Path: path})
	if !resp.OK || resp.BytesReturned != maxFileReadBytes || !resp.Truncated || resp.EOF {
		t.Fatalf("large ranged read = %+v", resp)
	}
	if len(resp.Output) != maxFileReadBytes {
		t.Fatalf("output bytes = %d, want %d", len(resp.Output), maxFileReadBytes)
	}
}

func TestWriteFileAtomicReplacementPreservesModeAndReturnsVersion(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "replace.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	resp := server.writeFile(Request{Path: path, Content: "new-content"})
	if !resp.OK || resp.Status != "written" || resp.FileVersion == "" || resp.FileSize != int64(len("new-content")) {
		t.Fatalf("write response = %+v", resp)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new-content" {
		t.Fatalf("content = %q", string(b))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestWriteFileOptimisticVersionRejectsConcurrentChange(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "optimistic.txt")
	if err := os.WriteFile(path, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	initial := server.readFile(Request{Path: path, Limit: 16})
	if !initial.OK {
		t.Fatalf("initial read = %+v", initial)
	}
	if err := os.WriteFile(path, []byte("v2-external"), 0600); err != nil {
		t.Fatal(err)
	}
	resp := server.writeFile(Request{Path: path, Content: "should-not-land", FileVersion: initial.FileVersion})
	if resp.OK || resp.ErrorCode != "file_changed" {
		t.Fatalf("stale optimistic write = %+v", resp)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "v2-external" {
		t.Fatalf("stale write changed destination: %q", string(b))
	}
}

func TestWriteFileMustNotExistAndMissingParent(t *testing.T) {
	server := newFileTestServer(t, nil)
	root := t.TempDir()
	path := filepath.Join(root, "new.txt")
	created := server.writeFile(Request{Path: path, Content: "created", MustNotExist: true})
	if !created.OK {
		t.Fatalf("must-not-exist create = %+v", created)
	}
	conflict := server.writeFile(Request{Path: path, Content: "again", MustNotExist: true})
	if conflict.OK || conflict.ErrorCode != "file_exists" {
		t.Fatalf("must-not-exist conflict = %+v", conflict)
	}
	missingParent := filepath.Join(root, "missing", "file.txt")
	resp := server.writeFile(Request{Path: missingParent, Content: "no hidden mkdir"})
	if resp.OK || resp.ErrorCode != "parent_unavailable" {
		t.Fatalf("missing-parent response = %+v", resp)
	}
	if _, err := os.Stat(filepath.Dir(missingParent)); !os.IsNotExist(err) {
		t.Fatalf("write_file created missing parent; err=%v", err)
	}
}

func TestWriteFileProtectedParentAliasRequiresApproval(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "protected-ai-server-agent")
	if err := os.Mkdir(protectedDir, 0700); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "alias-dir")
	if err := os.Symlink(protectedDir, aliasDir); err != nil {
		t.Fatal(err)
	}
	server := newFileTestServer(t, []string{protectedDir})
	aliasPath := filepath.Join(aliasDir, "config.txt")

	blocked := server.writeFile(Request{Path: aliasPath, Content: "blocked"})
	if blocked.OK || blocked.ErrorCode != "approval_required" {
		t.Fatalf("protected parent alias was not blocked: %+v", blocked)
	}
	approved := server.writeFile(Request{Path: aliasPath, Content: "approved", Approval: true})
	if !approved.OK {
		t.Fatalf("approved alias write = %+v", approved)
	}
	b, err := os.ReadFile(filepath.Join(protectedDir, "config.txt"))
	if err != nil || string(b) != "approved" {
		t.Fatalf("effective protected target content=%q err=%v", string(b), err)
	}
}

func TestWriteFileRejectsSymlinkAndSpecialDestination(t *testing.T) {
	server := newFileTestServer(t, nil)
	root := t.TempDir()
	victim := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(victim, []byte("victim"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	linkResp := server.writeFile(Request{Path: link, Content: "attack"})
	if linkResp.OK || linkResp.ErrorCode != "unsafe_file_type" {
		t.Fatalf("symlink write response = %+v", linkResp)
	}
	b, err := os.ReadFile(victim)
	if err != nil || string(b) != "victim" {
		t.Fatalf("symlink write changed victim: %q err=%v", string(b), err)
	}

	fifo := filepath.Join(root, "pipe")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	fifoResp := server.writeFile(Request{Path: fifo, Content: "attack"})
	if fifoResp.OK || fifoResp.ErrorCode != "unsafe_file_type" {
		t.Fatalf("FIFO write response = %+v", fifoResp)
	}
}

func TestWriteFileInputBoundLeavesDestinationUntouched(t *testing.T) {
	server := newFileTestServer(t, nil)
	path := filepath.Join(t.TempDir(), "bounded.txt")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	resp := server.writeFile(Request{Path: path, Content: strings.Repeat("z", maxFileWriteBytes+1)})
	if resp.OK || resp.ErrorCode != "input_too_large" {
		t.Fatalf("oversized write response = %+v", resp)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "keep" {
		t.Fatalf("oversized write changed destination: %q err=%v", string(b), err)
	}
}
