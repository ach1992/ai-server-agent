package systemexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewestTrustedVersionedBinarySelection(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"php", "php8.1", "php8.2", "php8.4", "php8.5", "php8.10",
		"php8.010", "php8.+11", "php8.5-debug", "php9.0",
		"python3", "python3.9", "python3.10", "python3.14", "python3.16",
		"python3.016", "python4.0",
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		label, name string
		major, min  int
		allowed     map[string]bool
		want        string
	}{
		{
			label: "php newest minor without numeric lexicographic sorting", name: "php", major: 8, min: 2,
			allowed: map[string]bool{"php8.1": true, "php8.2": true, "php8.4": true, "php8.5": true, "php8.10": true, "php8.010": true, "php9.0": true},
			want:    "php8.10",
		},
		{
			label: "php unsupported 8.1 and untrusted newest ignored", name: "php", major: 8, min: 2,
			allowed: map[string]bool{"php8.1": true, "php8.2": true, "php8.4": true, "php8.5": true, "php8.010": true, "php8.+11": true, "php9.0": true},
			want:    "php8.5",
		},
		{
			label: "no supported php interpreter", name: "php", major: 8, min: 2,
			allowed: map[string]bool{"php8.1": true, "php9.0": true},
		},
		{
			label: "python future minor and supported minimum", name: "python", major: 3, min: 10,
			allowed: map[string]bool{"python3.9": true, "python3.10": true, "python3.14": true, "python3.16": true, "python3.016": true, "python4.0": true},
			want:    "python3.16",
		},
		{
			label: "python missing supported interpreter", name: "python", major: 3, min: 10,
			allowed: map[string]bool{"python3.9": true, "python4.0": true},
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			got := newestVersionedPath(dir, entries, tc.name, tc.major, tc.min, func(path string) bool {
				return tc.allowed[filepath.Base(path)]
			})
			want := ""
			if tc.want != "" {
				want = filepath.Join(dir, tc.want)
			}
			if got != want {
				t.Fatalf("selected %q; want %q", got, want)
			}
		})
	}
	// Production trust is not relaxed for worker-owned binaries.
	if got := newestTrustedVersioned(dir, "php", 8, 2); got != "" {
		t.Fatalf("worker-owned PHP fixture was trusted: %s", got)
	}
}
