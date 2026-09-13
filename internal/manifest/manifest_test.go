package manifest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestFilesystemInfoReportsUsage(t *testing.T) {
	const blockSize uint64 = 1024 * 1024 * 1024
	info := filesystemInfo("/srv/ai-workspace", 100, 25, blockSize)
	if info.Path != "/srv/ai-workspace" {
		t.Fatalf("path = %q, want /srv/ai-workspace", info.Path)
	}
	if info.TotalBytes != 100*blockSize {
		t.Fatalf("total bytes = %d, want %d", info.TotalBytes, 100*blockSize)
	}
	if info.AvailableBytes != 25*blockSize {
		t.Fatalf("available bytes = %d, want %d", info.AvailableBytes, 25*blockSize)
	}
	if info.AvailablePercent != 25 {
		t.Fatalf("available percent = %d, want 25", info.AvailablePercent)
	}
	if info.Warning != "" {
		t.Fatalf("unexpected warning: %q", info.Warning)
	}
}

func TestFilesystemInfoWarnsWhenSpaceIsMateriallyConstrained(t *testing.T) {
	percentageConstrained := filesystemInfo("/srv/ai-workspace", 100, 9, 1024*1024*1024)
	if percentageConstrained.AvailablePercent != 9 {
		t.Fatalf("available percent = %d, want 9", percentageConstrained.AvailablePercent)
	}
	if !strings.Contains(percentageConstrained.Warning, "materially constrained") {
		t.Fatalf("warning = %q, want constrained-space warning", percentageConstrained.Warning)
	}

	bytesConstrained := filesystemInfo("/srv/ai-workspace", 100, 50, 1024*1024)
	if bytesConstrained.AvailableBytes >= lowDiskAvailableBytes {
		t.Fatalf("available bytes = %d, want less than threshold", bytesConstrained.AvailableBytes)
	}
	if !strings.Contains(bytesConstrained.Warning, "materially constrained") {
		t.Fatalf("warning = %q, want constrained-space warning", bytesConstrained.Warning)
	}
}

func TestWorkspaceFilesystemInfoFailureIsAdvisory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	info := workspaceFilesystemInfo(missing)
	if info.Path != missing {
		t.Fatalf("path = %q, want %q", info.Path, missing)
	}
	if info.TotalBytes != 0 || info.AvailableBytes != 0 || info.AvailablePercent != 0 {
		t.Fatalf("unexpected metrics for unavailable filesystem: %+v", info)
	}
	if !strings.Contains(info.Warning, "usage is unavailable") {
		t.Fatalf("warning = %q, want unavailable-space warning", info.Warning)
	}
}

func TestBuildReportsWorkspaceFilesystemAndWorkspaceHygiene(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceDir = t.TempDir()
	m := Build(cfg)
	if m.WorkspaceFilesystem.Path != cfg.WorkspaceDir {
		t.Fatalf("workspace filesystem path = %q, want %q", m.WorkspaceFilesystem.Path, cfg.WorkspaceDir)
	}
	if m.WorkspaceFilesystem.TotalBytes == 0 {
		t.Fatal("workspace filesystem total bytes must be reported on supported Linux hosts")
	}
	if m.WorkspaceFilesystem.AvailableBytes > m.WorkspaceFilesystem.TotalBytes {
		t.Fatalf("available bytes %d exceed total bytes %d", m.WorkspaceFilesystem.AvailableBytes, m.WorkspaceFilesystem.TotalBytes)
	}

	rules := strings.Join(m.Rules, "\n")
	for _, want := range []string{
		"is persistent",
		"temporary resources",
		"prefer git worktree",
		"Dirty, untracked, ambiguous, or unknown workspace state is not safe to delete",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("rules_for_ai missing %q", want)
		}
	}
}
