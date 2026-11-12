package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBoundedLargeStdioBurstIsConsumerSpecific(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	burst := bytes.Repeat([]byte("x"), 350<<10)
	for _, tc := range []struct {
		kind      string
		wantWhole bool
	}{
		{"browser", true}, {"lsp", true}, {"dap", false}, {"terminal", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			entry := &stdioSession{id: "test-large-" + tc.kind, kind: tc.kind, ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: owner.Workspace, done: make(chan struct{})}
			if err := s.sessions.reserve(entry); err != nil {
				t.Fatal(err)
			}
			if _, err := (sessionEventWriter{entry: entry, stream: "stdout"}).Write(burst); err != nil {
				t.Fatal(err)
			}
			limit, _ := stdioOutputCapacity(tc.kind)
			result, err := s.sessions.read(owner, entry.id, 0, limit)
			if err != nil {
				t.Fatal(err)
			}
			if result.Truncated == tc.wantWhole {
				t.Fatalf("unexpected retention loss for %s truncated=%v", tc.kind, result.Truncated)
			}
			if tc.wantWhole {
				var got []byte
				for _, e := range result.Events {
					got = append(got, e.Data...)
				}
				if !bytes.Equal(got, burst) {
					t.Fatalf("incomplete large output: got %d expected %d", len(got), len(burst))
				}
			}
			s.sessions.mu.Lock()
			delete(s.sessions.sessions, entry.id)
			s.sessions.mu.Unlock()
		})
	}
}

func TestLSPBurstParsesFramesIndividuallyAndSupports256KiB(t *testing.T) {
	s, owner, _ := testStdioBroker(t)
	entry := &stdioSession{id: "test-lsp-large-frames", kind: "lsp", ownerID: owner.PrincipalID, ownerClass: owner.PrincipalClass, workspace: owner.Workspace, done: make(chan struct{})}
	if err := s.sessions.reserve(entry); err != nil {
		t.Fatal(err)
	}
	makeFrame := func(content string) []byte {
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "window/logMessage", "params": map[string]any{"message": content}})
		if err != nil {
			t.Fatal(err)
		}
		return append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	}
	a, b := makeFrame(strings.Repeat("a", 180<<10)), makeFrame(strings.Repeat("b", 180<<10))
	if _, err := (sessionEventWriter{entry: entry, stream: "stdout"}).Write(append(a, b...)); err != nil {
		t.Fatal(err)
	}
	w := newLSPWire(s, owner, entry.id)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i, want := range []string{strings.Repeat("a", 180<<10), strings.Repeat("b", 180<<10)} {
		pkt, err := w.next(ctx)
		if err != nil {
			t.Fatalf("frame %d lost in burst: %v", i, err)
		}
		var data struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(pkt.Params, &data); err != nil || data.Message != want {
			t.Fatalf("frame %d differs from original payload: %v", i, err)
		}
	}
	nearLimit := []byte(`{"message":"` + strings.Repeat("z", lspMaxFrameBytes-len(`{"message":""}`)) + `"}`)
	frame := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(nearLimit))), nearLimit...)
	body, _, err := readLSPFrame(frame)
	if err != nil || len(body) != lspMaxFrameBytes {
		t.Fatalf("valid 256KiB LSP response rejected: %d %v", len(body), err)
	}
	if _, _, err = readLSPFrame([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", lspMaxFrameBytes+1))); err == nil {
		t.Fatal("unbounded LSP response allowed")
	}
}

func TestDAP256KiBFrameAnd128KiBNormalizedResult(t *testing.T) {
	receiver, sender := net.Pipe()
	defer receiver.Close()
	defer sender.Close()
	d := &dapState{conn: receiver}
	payload := strings.Repeat("z", 220<<10)
	written := make(chan error, 1)
	go func() {
		written <- dapFrame(sender, map[string]any{"seq": 1, "type": "event", "event": "output", "body": map[string]any{"output": payload}})
	}()
	packet, err := d.readFrame()
	if err != nil {
		t.Fatalf("valid DAP frame: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(packet, []byte(payload)) {
		t.Fatalf("DAP payload missing: %d bytes", len(packet))
	}
	body := json.RawMessage(`{"value":"` + strings.Repeat("x", 100<<10) + `"}`)
	if _, err := normalizeDebugResult("/workspace", body); err != nil {
		t.Fatalf("valid 100KiB result was dropped: %v", err)
	}
	body = json.RawMessage(`{"value":"` + strings.Repeat("x", 132<<10) + `"}`)
	if _, err := normalizeDebugResult("/workspace", body); err == nil {
		t.Fatal("unbounded DAP result accepted")
	}
}

func TestManagedBrowser64KiBScriptAnd256KiBJPEG(t *testing.T) {
	s, owner, _ := browserAuditFixture(t)
	// Fully framed JSON/stdio exercise, independent of a pinned Chromium install.
	script := `#!/usr/bin/python3
import sys,json,base64,hashlib
JPEG=base64.b64decode('__JPEG__')
def reply(v):
    print('ASA_BROWSER_SESSION '+json.dumps(v,separators=(',',':')),flush=True)
reply({'event':'ready','fit_quality':True,'capture_max_bytes':262144,'script_max_bytes':65536})
for line in sys.stdin:
    req=json.loads(line)
    if req.get('type')=='close':break
    nonce=req.get('nonce')
    if req.get('type')=='script':
        reply({'event':'result','nonce':nonce,'result':{'ok':True,'value':len(req['script']),'logs':[]}})
    elif req.get('type')=='capture':
        encoded=base64.b64encode(JPEG).decode('ascii')
        pieces=[encoded[i:i+8192] for i in range(0,len(encoded),8192)]
        reply({'event':'capture_meta','nonce':nonce,'mime':'image/jpeg','quality':req['quality'],'size':len(JPEG),'sha256':hashlib.sha256(JPEG).hexdigest(),'parts':len(pieces)})
        for index,part in enumerate(pieces):reply({'event':'capture_part','nonce':nonce,'index':index,'data':part})
        reply({'event':'capture_done','nonce':nonce})
`
	img := image.NewRGBA(image.Rect(0, 0, 900, 600))
	n := uint32(42)
	for y := 0; y < 600; y++ {
		for x := 0; x < 900; x++ {
			n ^= n << 13
			n ^= n >> 17
			n ^= n << 5
			img.SetRGBA(x, y, color.RGBA{R: uint8(n), G: uint8(n >> 8), B: uint8(n >> 16), A: 255})
		}
	}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, &jpeg.Options{Quality: 40}); err != nil {
		t.Fatal(err)
	}
	original := jpegData.Bytes()
	if len(original) <= 32<<10 || len(original) > browserCaptureMaxBytes {
		t.Fatalf("JPEG fixture %d bytes does not demonstrate new limit", len(original))
	}
	script = strings.Replace(script, "__JPEG__", base64.StdEncoding.EncodeToString(original), 1)
	if err := os.WriteFile(s.browserNodeBinary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	opened := s.browserSessionAction(ctx, owner)
	if !opened.OK {
		t.Fatalf("cannot open isolated Browser QA fixture: %+v", opened)
	}
	defer func() {
		_ = s.browserSessionAction(ctx, browserOwnerControl(owner, opened.SessionID, "browser_session_close"))
	}()
	action := browserOwnerControl(owner, opened.SessionID, "browser_session_script")
	prefix, suffix := "return 1; /*", "*/"
	// Syntactically valid JS with a 64 KiB comment and doubled JSON escapes.
	commentSize := managedBrowserScriptBytes - len(prefix) - len(suffix)
	action.Content = prefix + strings.Repeat(`\`, commentSize) + suffix
	if len(action.Content) != managedBrowserScriptBytes {
		t.Fatalf("bad script fixture: %d", len(action.Content))
	}
	ran := s.browserSessionAction(ctx, action)
	if !ran.OK || !strings.Contains(ran.Output, fmt.Sprintf(`"value":%d`, managedBrowserScriptBytes)) {
		t.Fatalf("full 64KiB Browser script was not delivered: %+v", ran)
	}
	action.Content += "x"
	if fail := s.browserSessionAction(ctx, action); fail.OK || fail.ErrorCode != "invalid_browser_script" {
		t.Fatalf("65KiB script accepted: %+v", fail)
	}
	capture := browserOwnerControl(owner, opened.SessionID, "browser_session_capture")
	capture.Content = `{"quality":40,"max_width":1024}`
	got := s.browserSessionAction(ctx, capture)
	if !got.OK {
		t.Fatalf("valid JPEG %d bytes was dropped or misframed: %+v", len(original), got)
	}
	output, err := base64.StdEncoding.Strict().DecodeString(got.Output)
	sum := sha256.Sum256(original)
	if err != nil || !bytes.Equal(output, original) || got.FileVersion != "sha256:"+hex.EncodeToString(sum[:]) || got.BytesReturned != int64(len(original)) {
		t.Fatalf("256KiB JPEG exact bytes/digest were not preserved: %v", err)
	}
	if image, err := jpeg.Decode(bytes.NewReader(output)); err != nil || image.Bounds() != img.Bounds() {
		t.Fatalf("captured JPEG is not fully decodable: %v", err)
	}
	if status := s.browserSessionAction(ctx, browserOwnerControl(owner, opened.SessionID, "browser_session_status")); !status.OK {
		t.Fatalf("browser session was poisoned: %+v", status)
	}
}

func TestLegacyBrowserRunnerCapacityIsTruthfulAndNonPoisoning(t *testing.T) {
	s, owner, id, _ := openCaptureFixture(t, "known_too_large")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	got := s.browserSessionAction(ctx, browserCaptureRequest(owner, id))
	if got.OK || got.ErrorCode != "too_large" || !strings.Contains(got.Error, "32 KiB") {
		t.Fatalf("old Browser runner cap misrepresented as 256 KiB: %+v", got)
	}
	if close := s.browserSessionAction(ctx, browserOwnerControl(owner, id, "browser_session_close")); !close.OK {
		t.Fatalf("old capture close failed: %+v", close)
	}

	old, principal, _ := browserAuditFixture(t)
	opened := old.browserSessionAction(ctx, principal)
	if !opened.OK {
		t.Fatalf("legacy Browser open failed: %+v", opened)
	}
	defer func() {
		_ = old.browserSessionAction(ctx, browserOwnerControl(principal, opened.SessionID, "browser_session_close"))
	}()
	action := browserOwnerControl(principal, opened.SessionID, "browser_session_script")
	action.Content = "return 1;/*" + strings.Repeat("x", (16<<10)) + "*/"
	result := old.browserSessionAction(ctx, action)
	if result.OK || result.ErrorCode != "browser_script_capacity_unavailable" || result.Status != "not_executed" || result.SessionID != opened.SessionID {
		t.Fatalf("legacy runner did not refuse oversized script before execution: %+v", result)
	}
	status := old.browserSessionAction(ctx, browserOwnerControl(principal, opened.SessionID, "browser_session_status"))
	if !status.OK || status.Status != "running" {
		t.Fatalf("rejected script poisoned an old compatible session: %+v", status)
	}
}

func TestLSPPushDiagnosticsAccepts128KiBButRejectsLarger(t *testing.T) {
	uri := "file:///workspace/a.go"
	w := &lspWire{opened: map[string]int{uri: 1}, published: map[string]json.RawMessage{}}
	makePacket := func(n int) lspPacket {
		params, _ := json.Marshal(map[string]any{"uri": uri, "version": 1, "diagnostics": []map[string]any{{"message": strings.Repeat("m", n)}}})
		return lspPacket{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: params}
	}
	if err := w.recordDiagnostics(makePacket(90 << 10)); err != nil {
		t.Fatalf("legitimate 90KiB diagnostics were dropped: %v", err)
	}
	if len(w.published[uri]) <= 16<<10 {
		t.Fatal("test did not exceed historical diagnostics budget")
	}
	if err := w.recordDiagnostics(makePacket(132 << 10)); err == nil {
		t.Fatal("unbounded diagnostics accepted")
	}
}

// The response budgets are upper limits, not a request to fill them. Verify
// complete, version-bound semantics above the old cap and explicit refusal
// above the current cap without returning partial success.
func TestSemantic128KiBNormalizedCrossFileResults(t *testing.T) {
	makeInput := func(size int) json.RawMessage {
		return json.RawMessage(`{"locations":[{"uri":"file:///workspace/main.go","details":"` + strings.Repeat("x", size) + `"}]}`)
	}
	full, err := normalizeCodeLocations("/workspace", makeInput(100<<10))
	if err != nil || len(full) <= 32<<10 || !bytes.Contains(full, []byte(`"uri":"main.go"`)) {
		t.Fatalf("legitimate 100KiB semantic output omitted: %d %v", len(full), err)
	}
	pinned, err := bindCodeLocationVersions(full, map[string]string{"main.go": "v1"})
	if err != nil || !bytes.Contains(pinned, []byte(`"file_version":"v1"`)) || len(pinned) <= 32<<10 {
		t.Fatalf("large semantic cross-file binding failed: %d %v", len(pinned), err)
	}
	_, err = normalizeCodeLocations("/workspace", makeInput(132<<10))
	if err == nil || !strings.Contains(err.Error(), "code_result_too_large") {
		t.Fatalf("oversized semantic result not explicitly refused: %v", err)
	}
}

func TestDAPEventQueueUsesActualBytesWithin128KiB(t *testing.T) {
	d := &dapState{stage: "running"}
	d.acceptEvent(dapPacket{Type: "event", Event: "output", Body: json.RawMessage(`{"output":"` + strings.Repeat("x", 96<<10) + `"}`)})
	if d.eventsDropped != 0 || len(d.events) != 1 || d.eventBytes <= 32<<10 || d.eventBytes >= dapMaxQueuedBytes {
		t.Fatalf("valid large DAP event lost: events=%d bytes=%d dropped=%d", len(d.events), d.eventBytes, d.eventsDropped)
	}
	d.acceptEvent(dapPacket{Type: "event", Event: "output", Body: json.RawMessage(`{"output":"` + strings.Repeat("y", 132<<10) + `"}`)})
	if d.eventsDropped != 1 || len(d.events) != 1 || d.eventBytes > dapMaxQueuedBytes {
		t.Fatalf("oversize event was not counted and bounded: events=%d bytes=%d dropped=%d", len(d.events), d.eventBytes, d.eventsDropped)
	}
}

func TestDAPStdioQueueRetainsLargeResponseWithoutPartialSuccess(t *testing.T) {
	q := newDAPStdoutQueue()
	data := bytes.Repeat([]byte("x"), 380<<10)
	if n, err := q.Write(data); err != nil || n != len(data) {
		t.Fatalf("bounded DAP queue rejected valid burst: %d %v", n, err)
	}
	received := make([]byte, len(data))
	n, err, _ := q.consume(received)
	if err != nil || n != len(data) || !bytes.Equal(received, data) {
		t.Fatalf("DAP stdio lost a valid 380 KiB response: %d %v", n, err)
	}
	if _, err := q.Write(bytes.Repeat([]byte("z"), dapStdioQueueLimit+1)); err != nil {
		t.Fatal(err)
	}
	if _, err, _ := q.consume(received); err == nil || !strings.Contains(err.Error(), "dap_stdio_output_lost") {
		t.Fatalf("DAP overflow not reported explicitly: %v", err)
	}
}
