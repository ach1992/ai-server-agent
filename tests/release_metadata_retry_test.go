package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real curl options in the installed updater's metadata helper,
// without depending on GitHub network availability or running privileged update.
func metadataFunction(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile("../update.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	start := strings.Index(source, "github_release_metadata(){\n")
	if start < 0 {
		t.Fatal("stable release metadata helper missing")
	}
	remaining := source[start:]
	end := strings.Index(remaining, "\n}\n")
	if end < 0 {
		t.Fatal("stable release metadata helper has no closing brace")
	}
	return remaining[:end+3]
}

func fetchMetadata(t *testing.T, function, url string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
	defer cancel()
	// Positional parameter is never interpolated as Bash source.
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -e\n"+function+"\ngithub_release_metadata \"$1\"\n", "bash", url)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded GitHub metadata request hung: %v", ctx.Err())
	}
	return output, err
}

func TestReleaseMetadataTransportRecoversTransientFailures(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed on this non-production test environment")
	}
	function := metadataFunction(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/tags/v0.1.14" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if requests.Add(1) == 1 {
			http.Error(w, "temporary upstream failure", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v0.1.14","immutable":true,"draft":false,"prerelease":false}`)
	}))
	defer server.Close()

	output, err := fetchMetadata(t, function, server.URL+"/releases/tags/v0.1.14")
	if err != nil {
		t.Fatalf("bounded transient failure was not recovered: %v: %s", err, output)
	}
	if requests.Load() != 2 || !strings.Contains(string(output), `"immutable":true`) {
		t.Fatalf("unexpected retries/result: count=%d, body=%s", requests.Load(), output)
	}
}

func TestReleaseMetadataTransportRefusesPersistentFailure(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed on this non-production test environment")
	}
	function := metadataFunction(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "upstream still unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	output, err := fetchMetadata(t, function, server.URL+"/releases/tags/v0.1.14")
	if err == nil {
		t.Fatalf("failed-open on persistent metadata failure: %s", output)
	}
	if count := requests.Load(); count < 2 || count > 4 {
		t.Fatalf("retry was unbounded or absent: count=%d; %s", count, output)
	}
}

func TestReleaseMetadataTransportRecoversDroppedConnection(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed on this non-production test environment")
	}
	function := metadataFunction(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("cannot simulate interrupted connection: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v0.1.14","immutable":true}`)
	}))
	defer server.Close()
	output, err := fetchMetadata(t, function, server.URL+"/releases/tags/v0.1.14")
	if err != nil || requests.Load() != 2 || !strings.Contains(string(output), `"immutable":true`) {
		t.Fatalf("bounded disconnected metadata request did not recover: count=%d err=%v output=%s", requests.Load(), err, output)
	}
}
