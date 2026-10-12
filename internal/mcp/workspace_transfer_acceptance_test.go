package mcp

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/credential"
	"github.com/ach1992/ai-server-agent/internal/executor"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Record actual HTTP entity-body bytes read by the transport. These are NOT
// TLS/TCP/IP on-wire bytes or ChatGPT model-context consumption.
type transferHTTPBytes struct {
	request  atomic.Int64
	response atomic.Int64
}

type transferCountedBody struct {
	io.ReadCloser
	target *atomic.Int64
}

func (r *transferCountedBody) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.target.Add(int64(n))
	return n, err
}

type transferMeasuredTransport struct {
	base  http.RoundTripper
	bytes *transferHTTPBytes
}

func (m transferMeasuredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if req.Body != nil {
		clone.Body = &transferCountedBody{ReadCloser: req.Body, target: &m.bytes.request}
	}
	res, err := m.base.RoundTrip(clone)
	if err == nil && res.Body != nil {
		res.Body = &transferCountedBody{ReadCloser: res.Body, target: &m.bytes.response}
	}
	return res, err
}

// Linux-specific bounded diagnostics for the isolated Agent and Executor.
// Unlike the go test process' RSS, these are from the actual subprocesses.
func transferProcessRSS(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kib, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return kib << 10
				}
			}
		}
	}
	return 0
}

// /proc/<pid>/stat fields 14 and 15 (utime, stime), indexed after the final
// process-command parenthesis. Report CPU *seconds* only when getconf CLK_TCK
// is available. The short-lived workspace helper processes are excluded.
func transferProcessCPUTicks(pid int) uint64 {
	if pid <= 0 {
		return 0
	}
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	close := bytes.LastIndexByte(content, ')')
	if close < 0 {
		return 0
	}
	fields := strings.Fields(string(content[close+1:]))
	if len(fields) < 13 {
		return 0
	}
	user, err1 := strconv.ParseUint(fields[11], 10, 64)
	system, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return user + system
}

// Sample file-backed disk allocation for disposable fixture files only.
// Anonymous O_TMPFILE inodes and other concurrent host activity are not
// included, so this must not be presented as peak total filesystem usage.
func transferFixtureDiskBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			total += stat.Blocks * 512
		}
		return nil
	})
	return total
}

// Both acceptance modes use a real authenticated official MCP SDK client, Unix
// executor, and worker helper. The opt-in installed mode launches the supplied
// *installed binary* in two disposable unprivileged processes on loopback; it
// never replaces or restarts either protected system service.
func TestDirectMCPTransferSourceAcceptance(t *testing.T) {
	runDirectMCPTransferAcceptance(t, "")
}

func TestDirectMCPTransferInstalledAcceptance(t *testing.T) {
	binary := os.Getenv("ASA_TRANSFER_ACCEPTANCE_BINARY")
	if binary == "" {
		t.Skip("set ASA_TRANSFER_ACCEPTANCE_BINARY to an authorized installed Agent binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("installed acceptance binary must be an absolute path")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("installed acceptance binary must be a regular accessible file: %v", err)
	}
	runDirectMCPTransferAcceptance(t, binary)
}

func runDirectMCPTransferAcceptance(t *testing.T, installedBinary string) {
	if testing.Short() {
		t.Skip("real worker and multi-MiB MCP acceptance")
	}
	if os.Geteuid() == 0 {
		t.Skip("must prove the worker path as an unprivileged user")
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t, "bearer")
	root := t.TempDir()
	cfg.WorkspaceDir = filepath.Join(root, "workspace")
	cfg.StateDir = filepath.Join(root, "state")
	cfg.LogDir = filepath.Join(root, "logs")
	cfg.ExecutorSocket = filepath.Join(root, "executor.sock")
	cfg.WorkerUser, cfg.AgentUser = current.Username, current.Username
	primaryToken := "mcp-token"
	secondaryToken := ""
	if installedBinary != "" {
		// The real installed-mode server authenticates two *distinct* test
		// principals. Never import the protected host's actual credentials.
		tokens := make([]string, 2)
		records := make([]credential.Record, 2)
		principals := []credential.Principal{
			{ID: "direct-default", Class: "direct", Name: "direct/default"},
			{ID: "mcp-gateway", Class: "gateway", Name: "mcp-gateway"},
		}
		for i := range tokens {
			raw := make([]byte, credential.TokenHexLength/2)
			if _, err := cryptorand.Read(raw); err != nil {
				t.Fatal(err)
			}
			tokens[i] = hex.EncodeToString(raw)
			verifier, err := credential.VerifyToken(tokens[i])
			if err != nil {
				t.Fatal(err)
			}
			records[i] = credential.Record{
				Principal: principals[i], VerifierAlgorithm: credential.VerifierAlgorithm,
				Verifier: verifier, CreatedAt: time.Now().UTC().Format(time.RFC3339), Enabled: true,
			}
		}
		storeData, err := json.Marshal(credential.Store{Version: credential.StoreVersion, Credentials: records})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "test-credentials.json")
		if err := os.WriteFile(path, storeData, 0600); err != nil {
			t.Fatal(err)
		}
		cfg.BearerTokenFile = ""
		cfg.CredentialStoreFile = path
		primaryToken, secondaryToken = tokens[0], tokens[1]
	}
	if installedBinary != "" {
		// Never bind the real test listener to the installed service port.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.ListenAddress = listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{cfg.WorkspaceDir, cfg.StateDir, cfg.LogDir, filepath.Join(cfg.StateDir, "worker-home")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	key := make([]byte, 32)
	if _, err := cryptorand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.ExecutorToken), "audit-fingerprint.key"), []byte("v1:"+hex.EncodeToString(key)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	binary := installedBinary
	if binary == "" {
		binary = filepath.Join(root, "ai-server-agent")
		build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/ai-server-agent")
		build.Dir = filepath.Join("..", "..")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build actual workspace helper/executor binary: %v: %s", err, output)
		}
	}
	// Record the binary under test, never the contents of any token file.
	binaryFile, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	binaryHasher := sha256.New()
	if _, err := io.Copy(binaryHasher, binaryFile); err != nil {
		_ = binaryFile.Close()
		t.Fatal(err)
	}
	if err := binaryFile.Close(); err != nil {
		t.Fatal(err)
	}
	binaryDigest := hex.EncodeToString(binaryHasher.Sum(nil))
	want := os.Getenv("ASA_TRANSFER_ACCEPTANCE_SHA256")
	if installedBinary != "" && len(want) != 64 {
		t.Fatal("installed acceptance requires ASA_TRANSFER_ACCEPTANCE_SHA256 with exact pinned 64-character digest")
	}
	if want != "" && binaryDigest != want {
		t.Fatalf("unexpected installed binary digest %s (expected %s)", binaryDigest, want)
	}
	t.Logf("acceptance binary=%s sha256=%s installed_mode=%t", binary, binaryDigest, installedBinary != "")

	// A real killed executor can be relaunched from the same durable state.
	// Keep the last process' cleanup function current after restart.
	var executorPID atomic.Int64
	startExecutor := func() func() {
		processCtx, stop := context.WithCancel(context.Background())
		command := exec.CommandContext(processCtx, binary, "-config", configPath, "executor")
		var processLog bytes.Buffer
		command.Stdout, command.Stderr = &processLog, &processLog
		if err := command.Start(); err != nil {
			stop()
			t.Fatal(err)
		}
		executorPID.Store(int64(command.Process.Pid))
		processDone := make(chan error, 1)
		go func() { processDone <- command.Wait() }()
		deadline := time.Now().Add(7 * time.Second)
		for {
			socket, dialErr := net.DialTimeout("unix", cfg.ExecutorSocket, 100*time.Millisecond)
			if dialErr == nil {
				_ = socket.Close()
				break
			}
			select {
			case err := <-processDone:
				stop()
				t.Fatalf("isolated executor exited: %v, log=%q", err, processLog.String())
			default:
			}
			if time.Now().After(deadline) {
				stop()
				t.Fatalf("fixture executor socket not ready: %v", dialErr)
			}
			time.Sleep(25 * time.Millisecond)
		}
		return func() {
			stop()
			select {
			case <-processDone:
			case <-time.After(5 * time.Second):
				t.Error("fixture executor did not terminate on cancel")
			}
		}
	}
	stopExecutor := startExecutor()
	t.Cleanup(func() { stopExecutor() })

	var endpoint string
	isolatedAgentPID := 0
	var httpClient *http.Client
	if installedBinary != "" {
		processCtx, stop := context.WithCancel(context.Background())
		command := exec.CommandContext(processCtx, binary, "-config", configPath, "serve")
		var processLog bytes.Buffer
		command.Stdout, command.Stderr = &processLog, &processLog
		if err := command.Start(); err != nil {
			stop()
			t.Fatal(err)
		}
		processDone := make(chan error, 1)
		go func() { processDone <- command.Wait() }()
		t.Cleanup(func() {
			stop()
			select {
			case <-processDone:
			case <-time.After(5 * time.Second):
				t.Error("fixture Agent did not terminate on cancel")
			}
		})
		baseURL := "http://" + cfg.ListenAddress
		endpoint = baseURL + cfg.MCPPath
		httpClient = &http.Client{Timeout: 150 * time.Second}
		deadline := time.Now().Add(7 * time.Second)
		for {
			response, healthErr := httpClient.Get(baseURL + cfg.HealthPath)
			if healthErr == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					break
				}
			}
			select {
			case err := <-processDone:
				t.Fatalf("isolated installed Agent exited: %v, log=%q", err, processLog.String())
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("isolated installed Agent health unavailable: %v", healthErr)
			}
			time.Sleep(25 * time.Millisecond)
		}
		isolatedAgentPID = command.Process.Pid
		t.Logf("isolated installed Agent running on loopback; AgentPID=%d", command.Process.Pid)
	} else {
		agent, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		httpServer := httptest.NewServer(agent.Handler())
		t.Cleanup(httpServer.Close)
		endpoint = httpServer.URL + cfg.MCPPath
		httpClient = httpServer.Client()
	}
	clientCtx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	meter := &transferHTTPBytes{}
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpClient.Transport = bearerRoundTripper{
		base: transferMeasuredTransport{base: transport, bytes: meter}, token: primaryToken,
	}
	var peakAgentRSS atomic.Int64
	var peakExecutorRSS atomic.Int64
	var peakFixtureDisk atomic.Int64
	var completedExecutorCPUTicks atomic.Uint64
	if installedBinary != "" {
		// The test client samples the running installed binary processes. The
		// high-water observations are approximate, not total host memory.
		sampleDone := make(chan struct{})
		agentPID := int64(0)
		if isolatedAgentPID > 0 {
			agentPID = int64(isolatedAgentPID)
		}
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				recordPeak := func(dst *atomic.Int64, observed int64) {
					for previous := dst.Load(); observed > previous; previous = dst.Load() {
						if dst.CompareAndSwap(previous, observed) {
							break
						}
					}
				}
				recordPeak(&peakAgentRSS, transferProcessRSS(int(agentPID)))
				recordPeak(&peakExecutorRSS, transferProcessRSS(int(executorPID.Load())))
				recordPeak(&peakFixtureDisk, transferFixtureDiskBytes(root))
				select {
				case <-sampleDone:
					return
				case <-ticker.C:
				}
			}
		}()
		t.Cleanup(func() { close(sampleDone) })
	}
	connect := func() *mcpsdk.ClientSession {
		t.Helper()
		client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "worker-transfer-source-acceptance", Version: "v1"}, nil)
		session, err := client.Connect(clientCtx, &mcpsdk.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: httpClient,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	session := connect()
	defer func() { _ = session.Close() }()

	if installedBinary != "" {
		tools, err := session.ListTools(clientCtx, &mcpsdk.ListToolsParams{})
		if err != nil {
			t.Fatalf("installed MCP tool listing: %v", err)
		}
		found := map[string]bool{}
		for _, tool := range tools.Tools {
			found[tool.Name] = true
		}
		for _, name := range []string{"workspace_transfer", "workspace_stat", "workspace_read", "workspace_write"} {
			if !found[name] {
				t.Fatalf("installed MCP catalog is missing %s", name)
			}
		}
		t.Logf("installed authenticated MCP SDK catalog: %d tools, transfer/stat/read/write present", len(tools.Tools))
		unauthenticated := &http.Client{Timeout: 5 * time.Second,
			Transport: bearerRoundTripper{token: "incorrect-acceptance-token"}}
		wrongClient := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "transfer-unauthorized-probe", Version: "v1"}, nil)
		denied, err := wrongClient.Connect(clientCtx, &mcpsdk.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: unauthenticated,
		}, nil)
		if err == nil {
			_ = denied.Close()
			t.Fatal("incorrect bearer was accepted by installed MCP service")
		}
	}

	callOn := func(targetSession *mcpsdk.ClientSession, tool string, args map[string]any) (*mcpsdk.CallToolResult, executor.Response) {
		t.Helper()
		result, err := targetSession.CallTool(clientCtx, &mcpsdk.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s MCP call: %v", tool, err)
		}
		var resp executor.Response
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("%s structured result is missing: %v", tool, err)
		}
		if resp.OK == result.IsError {
			t.Fatalf("%s MCP error hint and executor state disagree: ok=%t isError=%t", tool, resp.OK, result.IsError)
		}
		if len(result.Content) != 1 {
			t.Fatalf("%s missing text fallback", tool)
		}
		if _, ok := result.Content[0].(*mcpsdk.TextContent); !ok {
			t.Fatalf("%s unexpected text fallback type %T", tool, result.Content[0])
		}
		return result, resp
	}
	call := func(tool string, args map[string]any) (*mcpsdk.CallToolResult, executor.Response) {
		return callOn(session, tool, args)
	}
	var alternateSession *mcpsdk.ClientSession
	if installedBinary != "" {
		alternateHTTP := &http.Client{Timeout: 150 * time.Second,
			Transport: bearerRoundTripper{token: secondaryToken}}
		altClient := mcpsdk.NewClient(&mcpsdk.Implementation{
			Name: "transfer-isolation-test", Version: "v1",
		}, nil)
		alternateSession, err = altClient.Connect(clientCtx, &mcpsdk.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: alternateHTTP,
		}, nil)
		if err != nil {
			t.Fatalf("separate authenticated principal cannot connect: %v", err)
		}
		t.Cleanup(func() { _ = alternateSession.Close() })
	}

	for _, sample := range []struct {
		name    string
		size    int
		textual bool
	}{
		{"small-text", 32 << 10, true},
		{"binary-five-mib", (5 << 20) + 43, false},
		{"binary-thirtythree-mib", (33 << 20) + 13, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			started := time.Now()
			requestBefore, responseBefore := meter.request.Load(), meter.response.Load()
			raw := make([]byte, sample.size)
			if sample.textual {
				for offset := 0; offset < len(raw); offset++ {
					raw[offset] = "A"[0]
				}
			} else {
				_, _ = rand.New(rand.NewSource(7381)).Read(raw)
				raw[0], raw[len(raw)-1] = 0, 0xff
			}
			sum := sha256.Sum256(raw)
			digest := hex.EncodeToString(sum[:])
			path := sample.name + ".fixture"
			base := map[string]any{"workspace": cfg.WorkspaceDir, "path": path}
			begin := map[string]any{"workspace": cfg.WorkspaceDir, "path": path,
				"operation": "begin", "transfer_key": "source-client-" + sample.name + "-stable",
				"expected_size": len(raw), "sha256": digest, "must_not_exist": true}
			_, begun := call("workspace_transfer", begin)
			if !begun.OK || begun.TransferID == "" {
				t.Fatalf("begin: %+v", begun)
			}
			_, replayBegin := call("workspace_transfer", begin)
			if !replayBegin.OK || replayBegin.TransferID != begun.TransferID {
				t.Fatalf("idempotent begin: %+v", replayBegin)
			}

			follow := func(op string) map[string]any {
				return map[string]any{"workspace": cfg.WorkspaceDir, "path": path, "operation": op, "transfer_id": begun.TransferID}
			}
			if alternateSession != nil && sample.name == "small-text" {
				_, denied := callOn(alternateSession, "workspace_transfer", follow("status"))
				if denied.OK {
					t.Fatalf("other authenticated principal accessed staging: %+v", denied)
				}
				differentWorkspace := filepath.Join(cfg.WorkspaceDir, "other")
				if err := os.MkdirAll(differentWorkspace, 0700); err != nil {
					t.Fatal(err)
				}
				otherWorkspaceCall := follow("status")
				otherWorkspaceCall["workspace"] = differentWorkspace
				_, denied = call("workspace_transfer", otherWorkspaceCall)
				if denied.OK {
					t.Fatalf("same principal accessed transfer via another workspace: %+v", denied)
				}
			}
			chunks := 0
			for offset := 0; offset < len(raw); {
				end := offset + (256 << 10)
				if end > len(raw) {
					end = len(raw)
				}
				part := raw[offset:end]
				checksum := sha256.Sum256(part)
				args := follow("chunk")
				args["offset"] = offset
				args["chunk_base64"] = base64.StdEncoding.EncodeToString(part)
				args["chunk_sha256"] = hex.EncodeToString(checksum[:])
				if offset == 0 {
					invalid := follow("chunk")
					invalid["chunk_base64"] = args["chunk_base64"]
					invalid["chunk_sha256"] = strings.Repeat("0", 64)
					_, rejected := call("workspace_transfer", invalid)
					if rejected.OK || rejected.ErrorCode != "chunk_hash_mismatch" {
						t.Fatalf("invalid chunk digest unexpectedly advanced transfer: %+v", rejected)
					}
				}
				_, uploaded := call("workspace_transfer", args)
				if !uploaded.OK || uploaded.TransferredBytes != int64(end) || uploaded.NextOffset == nil || *uploaded.NextOffset != int64(end) {
					t.Fatalf("chunk receipt offset %d: %+v", offset, uploaded)
				}
				if offset == 0 {
					if installedBinary != "" && sample.name == "small-text" {
						wrongOffset := follow("chunk")
						wrongOffset["offset"] = int64(123)
						wrongOffset["chunk_base64"] = args["chunk_base64"]
						wrongOffset["chunk_sha256"] = args["chunk_sha256"]
						_, rejected := call("workspace_transfer", wrongOffset)
						if rejected.OK {
							t.Fatalf("out-of-order offset advanced transfer: %+v", rejected)
						}
						malformed := follow("chunk")
						malformed["offset"] = end
						malformed["chunk_base64"] = "!!!not-base64!!!"
						malformed["chunk_sha256"] = args["chunk_sha256"]
						_, rejected = call("workspace_transfer", malformed)
						if rejected.OK {
							t.Fatalf("malformed binary payload accepted: %+v", rejected)
						}
						_, current := call("workspace_transfer", follow("status"))
						if !current.OK || current.TransferredBytes != int64(end) {
							t.Fatalf("invalid input changed durable progress: %+v", current)
						}
					}
					_, replay := call("workspace_transfer", args)
					if !replay.OK || replay.TransferredBytes != uploaded.TransferredBytes {
						t.Fatalf("duplicate chunk changed progress: %+v", replay)
					}
					_ = session.Close()
					if installedBinary != "" && sample.name == "binary-five-mib" {
						// Real mid-upload executor process loss. The resumed client
						// must use the persisted staging journal, not in-memory state.
						completedExecutorCPUTicks.Add(transferProcessCPUTicks(int(executorPID.Load())))
						stopExecutor()
						stopExecutor = startExecutor()
						t.Log("executor killed and restarted with the same staging state")
					}
					session = connect() // a real client transport reconnect, same authenticated principal.
					_, recovered := call("workspace_transfer", follow("status"))
					if !recovered.OK || recovered.TransferredBytes != uploaded.TransferredBytes || recovered.TransferID != begun.TransferID {
						t.Fatalf("MCP reconnect lost durable worker identity: %+v", recovered)
					}
				}
				chunks++
				offset = end
			}

			_, before := call("workspace_stat", base)
			if before.OK {
				t.Fatal("target became visible before verified commit")
			}
			_, stat := call("workspace_transfer", follow("status"))
			if !stat.OK || stat.TransferredBytes != int64(len(raw)) {
				t.Fatalf("incomplete status: %+v", stat)
			}
			steps := 0
			var committed executor.Response
			for {
				_, committed = call("workspace_transfer", follow("commit"))
				if !committed.OK {
					t.Fatalf("commit failed: %+v", committed)
				}
				if committed.Status == "committed" {
					break
				}
				if committed.Status != "preparing" || committed.PreparedBytes <= 0 {
					t.Fatalf("non-progress preparation: %+v", committed)
				}
				_, before = call("workspace_stat", base)
				if before.OK {
					t.Fatal("incomplete destination published during preparation")
				}
				steps++
				if steps > (len(raw)/(4<<20))+2 {
					t.Fatal("commit failed to converge with bounded steps")
				}
			}
			if committed.FileVersion == "" {
				t.Fatalf("commit lacks file version: %+v", committed)
			}
			uploadedIn := time.Since(started)
			uploadBodyBytes := meter.request.Load() - requestBefore
			uploadReplyBytes := meter.response.Load() - responseBefore
			_, metadata := call("workspace_stat", base)
			if !metadata.OK || metadata.FileSize == nil || *metadata.FileSize != int64(len(raw)) || metadata.FileVersion != committed.FileVersion {
				t.Fatalf("stat/commit disagree: %+v %+v", committed, metadata)
			}

			// A small selected model-visible range must survive text-only fallback
			// even when its binary bytes cannot be rendered as UTF-8.
			previewArgs := map[string]any{"workspace": cfg.WorkspaceDir, "path": path, "limit": 4096, "file_version": metadata.FileVersion}
			visible, preview := call("workspace_read", previewArgs)
			text := visible.Content[0].(*mcpsdk.TextContent).Text
			var fallback executor.Response
			if err := json.Unmarshal([]byte(text), &fallback); err != nil || !fallback.OK || fallback.Output == "" {
				t.Fatalf("small text-only range is not visible: %v; fallback=%q", err, text)
			}
			if preview.NextOffset == nil || *preview.NextOffset != 4096 {
				t.Fatalf("small structured continuation absent: %+v", preview)
			}
			hasher := sha256.New()
			offset := int64(0)
			reads := 0
			for offset < int64(len(raw)) {
				args := map[string]any{"workspace": cfg.WorkspaceDir, "path": path,
					"offset": offset, "limit": 128 << 10, "file_version": metadata.FileVersion}
				result, window := call("workspace_read", args)
				if !window.OK || window.NextOffset == nil || *window.NextOffset <= offset || window.FileVersion != metadata.FileVersion {
					t.Fatalf("unusable egress offset %d: %+v", offset, window)
				}
				if window.BytesReturned >= 64<<10 {
					summary := result.Content[0].(*mcpsdk.TextContent).Text
					if !strings.Contains(summary, "structuredContent") || !strings.Contains(summary, "next_offset=") || strings.Contains(summary, window.Output) {
						t.Fatalf("large raw-byte range leaked into text fallback or lacked continuation")
					}
				}
				var payload []byte
				switch window.OutputEncoding {
				case "utf-8":
					payload = []byte(window.Output)
				case "base64":
					payload, err = base64.StdEncoding.DecodeString(window.Output)
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unexpected window encoding %s", window.OutputEncoding)
				}
				if int64(len(payload)) != *window.NextOffset-offset {
					t.Fatal("raw-byte window length/cursor mismatch")
				}
				_, _ = hasher.Write(payload)
				offset = *window.NextOffset
				reads++
			}
			if got := hex.EncodeToString(hasher.Sum(nil)); got != digest {
				t.Fatalf("complete direct-MCP readback digest %s != source %s", got, digest)
			}
			_, duplicateCommit := call("workspace_transfer", follow("commit"))
			if !duplicateCommit.OK || duplicateCommit.FileVersion != committed.FileVersion {
				t.Fatalf("committed terminal retry changed destination: %+v", duplicateCommit)
			}
			fullElapsed := time.Since(started)
			readElapsed := fullElapsed - uploadedIn
			t.Logf("transport+worker: bytes=%d sha256=%s file_version=%s upload_chunks=%d commit_calls=%d read_windows=%d upload_elapsed=%s full_elapsed=%s upload_MiB_per_second=%.2f retrieval_MiB_per_second=%.2f upload_http_body_bytes=%d upload_http_response_body_bytes=%d total_http_body_bytes_out=%d total_http_body_bytes_in=%d",
				len(raw), digest, committed.FileVersion, chunks, steps+1, reads, uploadedIn, fullElapsed,
				float64(len(raw))/(1<<20)/uploadedIn.Seconds(), float64(len(raw))/(1<<20)/readElapsed.Seconds(),
				uploadBodyBytes, uploadReplyBytes, meter.request.Load()-requestBefore, meter.response.Load()-responseBefore)
		})
	}

	if installedBinary != "" {
		t.Run("installed-failure-envelope", func(t *testing.T) {
			raw := []byte("safe failed-hash test payload")
			checksum := sha256.Sum256(raw)
			start := map[string]any{
				"workspace": cfg.WorkspaceDir, "path": "failed-hash.fixture",
				"operation": "begin", "transfer_key": "installed-client-wrong-full-hash",
				"expected_size": len(raw), "sha256": strings.Repeat("0", 64), "must_not_exist": true,
			}
			_, begun := call("workspace_transfer", start)
			if !begun.OK {
				t.Fatalf("failure-fixture begin: %+v", begun)
			}
			follow := func(op string) map[string]any {
				return map[string]any{"workspace": cfg.WorkspaceDir, "path": "failed-hash.fixture",
					"operation": op, "transfer_id": begun.TransferID}
			}
			chunk := follow("chunk")
			chunk["chunk_base64"] = base64.StdEncoding.EncodeToString(raw)
			chunk["chunk_sha256"] = hex.EncodeToString(checksum[:])
			_, uploaded := call("workspace_transfer", chunk)
			if !uploaded.OK {
				t.Fatalf("hash-fixture chunk: %+v", uploaded)
			}
			_, denied := call("workspace_transfer", follow("commit"))
			if denied.OK || denied.ErrorCode == "" {
				t.Fatalf("incorrect whole-object checksum published: %+v", denied)
			}
			_, visible := call("workspace_stat", map[string]any{
				"workspace": cfg.WorkspaceDir, "path": "failed-hash.fixture"})
			if visible.OK {
				t.Fatal("incorrect whole-object checksum partially published")
			}
			_, canceled := call("workspace_transfer", follow("abort"))
			if !canceled.OK {
				t.Fatalf("abort after hash failure: %+v", canceled)
			}

			path := "unsafe-link"
			outside := filepath.Join(root, "outside-workspace")
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(cfg.WorkspaceDir, path)); err != nil {
				t.Fatal(err)
			}
			badRequests := []struct{ name, path string }{
				{"traversal", "../out-of-workspace.fixture"},
				{"symlink-parent", path + "/escape.fixture"},
			}
			for _, tc := range badRequests {
				t.Run(tc.name, func(t *testing.T) {
					_, denied := call("workspace_transfer", map[string]any{
						"workspace": cfg.WorkspaceDir, "path": tc.path, "operation": "begin",
						"transfer_key":  "installed-path-rejection-" + tc.name,
						"expected_size": len(raw), "sha256": hex.EncodeToString(checksum[:]),
						"must_not_exist": true,
					})
					if denied.OK {
						t.Fatalf("unsafe path accepted: %+v", denied)
					}
				})
			}

			_, limited := call("workspace_transfer", map[string]any{
				"workspace": cfg.WorkspaceDir, "path": "oversized-staging.fixture",
				"operation": "begin", "transfer_key": "installed-capacity-admission-denial",
				"expected_size": int64(513) << 20,
				"sha256":        hex.EncodeToString(checksum[:]), "must_not_exist": true,
			})
			if limited.OK {
				t.Fatalf("transfer staging exceeded effective quota: %+v", limited)
			}

			base := map[string]any{"workspace": cfg.WorkspaceDir, "path": "stale-version.fixture"}
			_, first := call("workspace_write", map[string]any{
				"workspace": cfg.WorkspaceDir, "path": "stale-version.fixture",
				"content": "one", "must_not_exist": true,
			})
			if !first.OK {
				t.Fatalf("version fixture creation: %+v", first)
			}
			_, previous := call("workspace_stat", base)
			if !previous.OK || previous.FileVersion == "" {
				t.Fatalf("version fixture stat: %+v", previous)
			}
			_, replacement := call("workspace_write", map[string]any{
				"workspace": cfg.WorkspaceDir, "path": "stale-version.fixture",
				"content": "two", "file_version": previous.FileVersion,
			})
			if !replacement.OK {
				t.Fatalf("version fixture replacement: %+v", replacement)
			}
			_, stale := call("workspace_read", map[string]any{
				"workspace": cfg.WorkspaceDir, "path": "stale-version.fixture",
				"limit": 64, "file_version": previous.FileVersion,
			})
			if stale.OK {
				t.Fatalf("stale file_version returned new bytes: %+v", stale)
			}
			t.Log("failure envelope: invalid bearer, principal/workspace, wrong chunk offset and base64, full SHA256, unsafe paths, quota, stale version PASS")
		})
	}

	if installedBinary != "" {
		t.Run("separate-cli-process-resume-and-download", func(t *testing.T) {
			// This is the independently usable example client. Begin and
			// upload a chunk in this test process, then exit its SDK session
			// and finish in an actual *different* OS process.
			payload := make([]byte, (768<<10)+19)
			if _, err := rand.New(rand.NewSource(92817)).Read(payload); err != nil {
				t.Fatal(err)
			}
			payload[0] = 0
			payload[len(payload)-1] = 0xff
			sum := sha256.Sum256(payload)
			hash := hex.EncodeToString(sum[:])
			path := "standalone-client-download.fixture"
			key := "standalone-client-resume-operator-test"
			begin := map[string]any{"workspace": cfg.WorkspaceDir, "path": path,
				"operation": "begin", "transfer_key": key, "expected_size": len(payload),
				"sha256": hash, "must_not_exist": true}
			_, receipt := call("workspace_transfer", begin)
			if !receipt.OK || receipt.TransferID == "" {
				t.Fatalf("cli pre-stage: %+v", receipt)
			}
			firstChunk := payload[:256<<10]
			digest := sha256.Sum256(firstChunk)
			_, preloaded := call("workspace_transfer", map[string]any{
				"workspace": cfg.WorkspaceDir, "path": path, "operation": "chunk",
				"transfer_id": receipt.TransferID, "chunk_base64": base64.StdEncoding.EncodeToString(firstChunk),
				"chunk_sha256": hex.EncodeToString(digest[:]),
			})
			if !preloaded.OK || preloaded.TransferredBytes != int64(len(firstChunk)) {
				t.Fatalf("cli pre-stage chunk: %+v", preloaded)
			}
			source := filepath.Join(root, "example-binary-input.fixture")
			output := filepath.Join(root, "example-binary-retrieved.fixture")
			if err := os.WriteFile(source, payload, 0600); err != nil {
				t.Fatal(err)
			}
			clientBinary := filepath.Join(root, "example-transfer-client")
			build := exec.Command("go", "build", "-trimpath", "-o", clientBinary, "./examples/workspace-transfer-client")
			build.Dir = filepath.Join("..", "..")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build CLI consumer: %v %s", err, out)
			}
			cli := func() ([]byte, error) {
				args := []string{
					"--endpoint", endpoint, "--workspace", cfg.WorkspaceDir, "--path", path,
					"--key", key, "--input", source, "--output", output, "--call-timeout", "2m",
				}
				command := exec.CommandContext(clientCtx, clientBinary, args...)
				command.Env = append(os.Environ(), "ASA_MCP_TOKEN="+primaryToken)
				return command.CombinedOutput()
			}
			result, err := cli()
			if err != nil {
				t.Fatalf("independent CLI failed to resume: %v output=%s", err, result)
			}
			var summary struct {
				Bytes        int64  `json:"bytes"`
				SHA256       string `json:"sha256"`
				FileVersion  string `json:"file_version"`
				ResumedFrom  int64  `json:"resumed_from"`
				UploadChunks int    `json:"upload_chunks"`
				ReadWindows  int    `json:"read_windows"`
			}
			if err := json.Unmarshal(result, &summary); err != nil {
				t.Fatalf("CLI summary malformed: %v output=%s", err, result)
			}
			if summary.Bytes != int64(len(payload)) || summary.SHA256 != hash ||
				summary.ResumedFrom != int64(len(firstChunk)) || summary.UploadChunks != 3 ||
				summary.FileVersion == "" || summary.ReadWindows != 7 {
				t.Fatalf("CLI didn't finish bounded resume and download: %+v", summary)
			}
			reconstructed, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(reconstructed, payload) {
				t.Fatal("standalone client output bytes differ from source")
			}
			if failed, err := cli(); err == nil {
				t.Fatalf("CLI silently overwrote an existing downloaded file: %s", failed)
			}
			after, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(after, payload) {
				t.Fatal("output changed after repeated CLI with existing target")
			}
			t.Logf("independent CLI process: bytes=%d sha256=%s file_version=%s resumed_at=%d upload_chunks=%d read_windows=%d safe_no_overwrite=true",
				summary.Bytes, summary.SHA256, summary.FileVersion, summary.ResumedFrom, summary.UploadChunks, summary.ReadWindows)
		})
	}

	// A competing worker client cannot be overwritten by a stale staged
	// destination precondition, even when all upload chunks were accepted.
	t.Run("concurrent-destination-wins", func(t *testing.T) {
		const path = "concurrent-writer.fixture"
		body := []byte("staged file must never replace a later independent writer")
		digest := sha256.Sum256(body)
		beginArgs := map[string]any{"workspace": cfg.WorkspaceDir, "path": path,
			"operation": "begin", "transfer_key": "source-client-raced-destination",
			"expected_size": len(body), "sha256": hex.EncodeToString(digest[:]), "must_not_exist": true}
		_, begun := call("workspace_transfer", beginArgs)
		if !begun.OK || begun.TransferID == "" {
			t.Fatalf("begin: %+v", begun)
		}
		follow := func(op string) map[string]any {
			return map[string]any{"workspace": cfg.WorkspaceDir, "path": path, "operation": op, "transfer_id": begun.TransferID}
		}
		chunk := follow("chunk")
		chunk["chunk_base64"] = base64.StdEncoding.EncodeToString(body)
		chunk["chunk_sha256"] = hex.EncodeToString(digest[:])
		_, received := call("workspace_transfer", chunk)
		if !received.OK {
			t.Fatalf("stage conflicting body: %+v", received)
		}
		_, winner := call("workspace_write", map[string]any{"workspace": cfg.WorkspaceDir, "path": path,
			"content": "independent writer", "must_not_exist": true})
		if !winner.OK {
			t.Fatalf("independent writer rejected: %+v", winner)
		}
		_, conflicted := call("workspace_transfer", follow("commit"))
		if conflicted.OK || conflicted.ErrorCode != "file_exists" {
			t.Fatalf("stale begin unexpectedly published: %+v", conflicted)
		}
		_, content := call("workspace_read", map[string]any{"workspace": cfg.WorkspaceDir, "path": path, "limit": 64})
		if !content.OK || content.Output != "independent writer" {
			t.Fatalf("independent destination changed after transfer rejection: %+v", content)
		}
		_, aborted := call("workspace_transfer", follow("abort"))
		if !aborted.OK {
			t.Fatalf("abort conflicted staged ingress: %+v", aborted)
		}
	})
	if installedBinary != "" {
		t.Logf("observed isolated installed subprocess peak RSS bytes: Agent=%d Executor=%d; visible fixture allocated disk peak=%d (50ms sampling, excludes helper/client and unnamed inode disk)",
			peakAgentRSS.Load(), peakExecutorRSS.Load(), peakFixtureDisk.Load())
		cpuTicks := transferProcessCPUTicks(isolatedAgentPID)
		execTicks := completedExecutorCPUTicks.Load() + transferProcessCPUTicks(int(executorPID.Load()))
		ticksPerSecondBytes, err := exec.Command("getconf", "CLK_TCK").Output()
		if err == nil {
			ticksPerSecond, parseErr := strconv.ParseFloat(strings.TrimSpace(string(ticksPerSecondBytes)), 64)
			if parseErr == nil && ticksPerSecond > 0 {
				t.Logf("observed isolated subprocess CPU seconds: Agent=%.3f Executor=%.3f (excludes helper and client)",
					float64(cpuTicks)/ticksPerSecond, float64(execTicks)/ticksPerSecond)
			}
		}
		if primaryToken == secondaryToken || secondaryToken == "" {
			t.Fatal("different-principal acceptance fixture was not configured")
		}
	}
}
