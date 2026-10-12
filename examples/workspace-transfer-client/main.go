// Example authenticated MCP transfer client. This is deliberately an ordinary
// SDK consumer, not a second Agent API or a privileged file writer.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ach1992/ai-server-agent/internal/executor"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	uploadWindow   = 256 << 10
	downloadWindow = 128 << 10
)

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header = req.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(copy)
}

type client struct {
	session *mcp.ClientSession
	timeout time.Duration
}

func (c client) call(tool string, args map[string]any) (executor.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return executor.Response{}, fmt.Errorf("%s completion unknown (%w); rerun with the SAME --key to reconcile", tool, err)
	}
	var reply executor.Response
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return reply, fmt.Errorf("decode %s result: %w", tool, err)
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return reply, fmt.Errorf("decode structured %s response: %w", tool, err)
	}
	if result.IsError || !reply.OK {
		return reply, fmt.Errorf("%s: %s (%s; retryable=%t)", tool, reply.Error, reply.ErrorCode, reply.Retryable)
	}
	return reply, nil
}

func shaOfFile(input *os.File) (string, int64, error) {
	info, err := input.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", 0, errors.New("input must be a nonempty regular file")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, input)
	if err != nil {
		return "", 0, err
	}
	if n != info.Size() {
		return "", 0, errors.New("input file changed size while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

// Completion publishes to a new local name ONLY after fully checked
// reconstruction. The existing local file is never overwritten.
func download(c client, workspace, path, output, version, expectedSHA string, size int64) (int, error) {
	meta, err := c.call("workspace_stat", map[string]any{"workspace": workspace, "path": path, "file_version": version})
	if err != nil {
		return 0, err
	}
	if meta.FileVersion != version || meta.FileSize == nil || *meta.FileSize != size {
		return 0, errors.New("remote file identity or size differs from committed receipt")
	}
	dir := filepath.Dir(output)
	tmp, err := os.CreateTemp(dir, ".asa-transfer-part-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	var offset int64
	calls := 0
	for offset < size {
		limit := int64(downloadWindow)
		if size-offset < limit {
			limit = size - offset
		}
		r, err := c.call("workspace_read", map[string]any{
			"workspace": workspace, "path": path, "offset": offset,
			"limit": int(limit), "file_version": version,
		})
		if err != nil {
			return calls, err
		}
		if r.FileVersion != version || r.NextOffset == nil || *r.NextOffset <= offset ||
			*r.NextOffset > size || r.BytesReturned != *r.NextOffset-offset {
			return calls, errors.New("unexpected version, size or offset in remote range")
		}
		var data []byte
		switch r.OutputEncoding {
		case "base64":
			data, err = base64.StdEncoding.DecodeString(r.Output)
		case "utf-8":
			data = []byte(r.Output)
		default:
			return calls, fmt.Errorf("unsupported range encoding %q", r.OutputEncoding)
		}
		if err != nil || int64(len(data)) != r.BytesReturned {
			return calls, errors.New("malformed or incomplete ranged payload")
		}
		if _, err = tmp.Write(data); err != nil {
			return calls, err
		}
		_, _ = hash.Write(data)
		offset = *r.NextOffset
		calls++
		if r.EOF != nil && *r.EOF && offset != size {
			return calls, errors.New("premature remote EOF")
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedSHA {
		return calls, errors.New("download SHA256 differs from original input")
	}
	if err := tmp.Sync(); err != nil {
		return calls, err
	}
	if err := tmp.Close(); err != nil {
		return calls, err
	}
	if err := os.Link(tmp.Name(), output); err != nil {
		return calls, fmt.Errorf("publish download without overwrite: %w", err)
	}
	return calls, nil
}

func validateEndpoint(value string) error {
	u, err := url.Parse(value)
	if err != nil || u == nil || u.Host == "" || u.User != nil {
		return errors.New("endpoint must be an absolute MCP URL without embedded credentials")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme != "http" || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return errors.New("HTTPS required except for isolated localhost/loopback test endpoints")
	}
	return nil
}

func run() error {
	var endpoint, workspace, path, inputPath, outputPath, stableKey string
	var timeout time.Duration
	flag.StringVar(&endpoint, "endpoint", "", "Agent MCP Streamable HTTP endpoint URL")
	flag.StringVar(&workspace, "workspace", "", "Authorized server worker workspace")
	flag.StringVar(&path, "path", "", "Relative destination file path in workspace")
	flag.StringVar(&inputPath, "input", "", "Local source file to upload")
	flag.StringVar(&outputPath, "output", "", "New local output path for reconstructed file (must not exist)")
	flag.StringVar(&stableKey, "key", "", "Caller-chosen 16..128 character stable retry/resume key")
	flag.DurationVar(&timeout, "call-timeout", 2*time.Minute, "Maximum duration of one bounded MCP call")
	flag.Parse()
	if endpoint == "" || workspace == "" || path == "" || inputPath == "" || outputPath == "" || stableKey == "" {
		return errors.New("required: --endpoint --workspace --path --input --output --key")
	}
	if err := validateEndpoint(endpoint); err != nil {
		return err
	}
	if timeout <= 0 {
		return errors.New("--call-timeout must be positive")
	}
	if filepath.Clean(inputPath) == filepath.Clean(outputPath) {
		return errors.New("--input and --output must be different paths")
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return errors.New("--output already exists; refuse overwrite")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check output destination: %w", err)
	}
	token := strings.TrimSpace(os.Getenv("ASA_MCP_TOKEN"))
	if token == "" {
		return errors.New("ASA_MCP_TOKEN environment variable required; do not include credentials in URL/argv")
	}
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()
	sum, size, err := shaOfFile(in)
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Transport:     bearerTransport{base: http.DefaultTransport, token: token},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	sdkClient := mcp.NewClient(&mcp.Implementation{Name: "asa-workspace-transfer-example", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	session, err := sdkClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient}, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("authenticated MCP connect: %w", err)
	}
	defer session.Close()
	c := client{session: session, timeout: timeout}
	started := time.Now()
	base := map[string]any{"workspace": workspace, "path": path}
	begin := map[string]any{
		"workspace": workspace, "path": path, "operation": "begin",
		"transfer_key": stableKey, "sha256": sum, "expected_size": size,
		"must_not_exist": true,
	}
	created, err := c.call("workspace_transfer", begin)
	if err != nil {
		return err
	}
	if created.TransferID == "" {
		return errors.New("missing stable transfer ID")
	}
	follow := func(op string) map[string]any {
		return map[string]any{"workspace": workspace, "path": path,
			"operation": op, "transfer_id": created.TransferID}
	}
	state, err := c.call("workspace_transfer", follow("status"))
	if err != nil {
		return err
	}
	startOffset := state.TransferredBytes
	uploadCalls := 0
	if state.Status != "committed" {
		if startOffset < 0 || startOffset > size {
			return errors.New("invalid resumed upload offset")
		}
		if _, err := in.Seek(startOffset, io.SeekStart); err != nil {
			return err
		}
		buf := make([]byte, uploadWindow)
		for offset := startOffset; offset < size; {
			n, err := io.ReadFull(in, buf[:min(int64(len(buf)), size-offset)])
			if err != nil {
				return fmt.Errorf("read local upload chunk: %w", err)
			}
			chunk := buf[:n]
			chunkDigest := sha256.Sum256(chunk)
			arg := follow("chunk")
			arg["offset"] = offset
			arg["chunk_base64"] = base64.StdEncoding.EncodeToString(chunk)
			arg["chunk_sha256"] = hex.EncodeToString(chunkDigest[:])
			reply, err := c.call("workspace_transfer", arg)
			if err != nil {
				return err
			}
			if reply.NextOffset == nil || *reply.NextOffset != offset+int64(n) {
				return errors.New("server returned unexpected chunk offset; rerun with same key")
			}
			offset = *reply.NextOffset
			uploadCalls++
		}
		// Each commit call prepares at most 4 MiB. The bound below scales
		// with the requested object and allows room for reconciliation.
		var previous int64
		for attempts := int64(0); attempts <= size/(4<<20)+8; attempts++ {
			reply, err := c.call("workspace_transfer", follow("commit"))
			if err != nil {
				return err
			}
			state = reply
			if state.Status == "committed" {
				break
			}
			if state.Status != "preparing" || state.PreparedBytes <= previous {
				return errors.New("commit returned no durable forward progress")
			}
			previous = state.PreparedBytes
		}
		if state.Status != "committed" {
			return errors.New("bounded commit could not converge; rerun with same key")
		}
	}
	if state.FileVersion == "" {
		return errors.New("committed transfer returned no final file_version")
	}
	// Verify current file metadata before the version-pinned ranged egress.
	if _, err := c.call("workspace_stat", map[string]any{
		"workspace": base["workspace"], "path": base["path"],
		"file_version": state.FileVersion,
	}); err != nil {
		return err
	}
	readCalls, err := download(c, workspace, path, outputPath, state.FileVersion, sum, size)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"bytes": size, "sha256": sum, "file_version": state.FileVersion,
		"resumed_from": startOffset, "upload_chunks": uploadCalls,
		"read_windows": readCalls, "output": outputPath,
		"elapsed_ms": time.Since(started).Milliseconds(),
	})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "transfer failed:", err)
		os.Exit(1)
	}
}
