package manifest

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestFilesystemInfoReportsUsage(t *testing.T) {
	info := filesystemInfo("/", 100, 25, 4096)
	if info.Path != "/" {
		t.Fatalf("path = %q, want /", info.Path)
	}
	if info.TotalBytes != 409600 {
		t.Fatalf("total bytes = %d, want 409600", info.TotalBytes)
	}
	if info.AvailableBytes != 102400 {
		t.Fatalf("available bytes = %d, want 102400", info.AvailableBytes)
	}
	if info.UsedPercent != 75 {
		t.Fatalf("used percent = %d, want 75", info.UsedPercent)
	}
	if info.Warning != "" {
		t.Fatalf("unexpected warning: %q", info.Warning)
	}
}

func TestFilesystemInfoWarnsWhenRootIsNinetyPercentUsed(t *testing.T) {
	info := filesystemInfo("/", 100, 10, 4096)
	if info.UsedPercent != 90 {
		t.Fatalf("used percent = %d, want 90", info.UsedPercent)
	}
	if !strings.Contains(info.Warning, "at least 90% utilized") {
		t.Fatalf("warning = %q, want low-disk warning", info.Warning)
	}
}

func TestBuildReportsRootFilesystemAndWorkspaceHygiene(t *testing.T) {
	m := Build(config.Default())
	if m.RootFilesystem.Path != "/" {
		t.Fatalf("root filesystem path = %q, want /", m.RootFilesystem.Path)
	}
	if m.RootFilesystem.TotalBytes == 0 {
		t.Fatal("root filesystem total bytes must be reported on supported Linux hosts")
	}
	if m.RootFilesystem.AvailableBytes > m.RootFilesystem.TotalBytes {
		t.Fatalf("available bytes %d exceed total bytes %d", m.RootFilesystem.AvailableBytes, m.RootFilesystem.TotalBytes)
	}

	rules := strings.Join(m.Rules, "\n")
	for _, want := range []string{
		"workspace is persistent",
		"prefer git worktree",
		"Dirty, untracked, ambiguous, or unknown workspace state is not safe to delete",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("rules_for_ai missing %q", want)
		}
	}
}
