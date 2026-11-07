package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBrowserSessionWriteBoundaryABI(t *testing.T) {
	for _, abi := range []uintptr{0, 1, 2} {
		if rights, err := browserSessionWriteAccess(abi); err == nil || rights != 0 ||
			!strings.Contains(err.Error(), "ABI 3") {
			t.Fatalf("ABI %d must explicitly reject incomplete truncation protection: rights=%d err=%v", abi, rights, err)
		}
	}
	for _, abi := range []uintptr{3, 4, 8} {
		rights, err := browserSessionWriteAccess(abi)
		if err != nil || rights&unix.LANDLOCK_ACCESS_FS_TRUNCATE == 0 ||
			rights&unix.LANDLOCK_ACCESS_FS_WRITE_FILE == 0 {
			t.Fatalf("ABI %d must include both write and truncate: rights=%d err=%v", abi, rights, err)
		}
	}
}

// The subprocess uses the exact production helper and real pinned Node,
// while all files and identities are disposable synthetic fixtures.
func TestBrowserSessionWriteBoundaryChild(t *testing.T) {
	if os.Getenv("ASA_BROWSER_WRITE_TEST_HELPER") != "1" {
		return
	}
	node := os.Getenv("ASA_BROWSER_WRITE_TEST_NODE")
	script := os.Getenv("ASA_BROWSER_WRITE_TEST_SCRIPT")
	if node == "" || script == "" {
		os.Exit(2)
	}
	os.Exit(RunBrowserSessionHelper([]string{node, "--input-type=module", "-e", script}))
}

func TestBrowserSessionWriteBoundaryAcrossPeers(t *testing.T) {
	engine := os.Getenv("AI_SERVER_AGENT_BROWSER_FLOW_RUNTIME")
	if engine == "" {
		t.Skip("opt-in pinned Node acceptance required")
	}
	node := filepath.Join(engine, "node", "bin", "node")
	if info, err := os.Stat(node); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("missing pinned Node: %v", err)
	}
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0,
		uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION))
	if errno != 0 || abi < 3 {
		t.Skip("host does not provide Landlock ABI 3 required for isolated Browser truncation denial")
	}
	scratch := filepath.Join(t.TempDir(), "browser")
	if err := os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	profileA, profileB := "asa-profile-"+strings.Repeat("a", 32), "asa-profile-"+strings.Repeat("b", 32)
	traceA, traceB := "asa-trace-"+strings.Repeat("a", 32), "asa-trace-"+strings.Repeat("b", 32)
	ownA, ownB := filepath.Join(scratch, profileA), filepath.Join(scratch, profileB)
	peerState := filepath.Join(ownB, "synthetic-cookie.txt")
	run := func(ctx context.Context, profile, trace, script string) *exec.Cmd {
		t.Helper()
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrowserSessionWriteBoundaryChild$")
		child.Env = append(os.Environ(),
			"ASA_BROWSER_WRITE_TEST_HELPER=1",
			"ASA_BROWSER_WRITE_TEST_NODE="+node,
			"ASA_BROWSER_WRITE_TEST_SCRIPT="+script,
			"ASA_BROWSER_RECLAIM_BY_EXECUTOR=1",
			"ASA_BROWSER_SESSION_PROFILE="+profile,
			"ASA_BROWSER_SESSION_TRACE="+trace,
			"TMPDIR="+filepath.Join(scratch, profile),
			"HOME="+filepath.Join(scratch, profile))
		return child
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// B stays alive and retains its synthetic session cookie while A executes.
	bScript := fmt.Sprintf(`import {writeFileSync,readFileSync} from 'node:fs';
writeFileSync(%q,'B-ORIGINAL');process.stdout.write('B_READY\n');
await new Promise(resolve=>process.stdin.once('data',resolve));
process.stdout.write('B_STATE='+readFileSync(%q,'utf8')+'\n');`, peerState, peerState)
	b := run(ctx, profileB, traceB, bScript)
	bIn, err := b.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	bOut, err := b.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var bErr bytes.Buffer
	b.Stderr = &bErr
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = b.Process.Kill()
		_ = b.Wait()
	}()
	reader := bufio.NewReader(bOut)
	ready, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "B_READY" {
		t.Fatalf("peer fixture not ready (%v): %q %s", err, ready, bErr.String())
	}
	aScript := fmt.Sprintf(`import {writeFileSync,readFileSync,rmSync,openSync,closeSync,truncateSync,constants} from 'node:fs';
writeFileSync(%q,'A-OWN-OK');
let denied=false;
try { writeFileSync(%q,'A-DAMAGED'); } catch (e) { denied=e.code==='EACCES'||e.code==='EPERM'; }
let cannotDelete=false;
try { rmSync(%q,{recursive:true,force:true}); } catch(e) { cannotDelete=e.code==='EACCES'||e.code==='EPERM'; }
// O_RDONLY|O_TRUNC does not require write permission; ABI 2 cannot deny it.
let cannotTruncate=false;
try { const fd=openSync(%q,constants.O_RDONLY|constants.O_TRUNC); closeSync(fd); }
catch(e) { cannotTruncate=e.code==='EACCES'||e.code==='EPERM'; }
truncateSync(%q,1); // own-session truncation stays available
// Reading is deliberately not confined: this is an integrity-only boundary.
const peerReadable=readFileSync(%q,'utf8')==='B-ORIGINAL';
if (!denied || !cannotDelete || !cannotTruncate || !peerReadable || readFileSync(%q,'utf8')!=='A') process.exitCode=10;
else {
  process.env.PLAYWRIGHT_BROWSERS_PATH=%q;
  const {chromium}=await import(%q);
  const context=await chromium.launchPersistentContext(%q,{headless:true,acceptDownloads:false});
  try {
    const page=context.pages()[0]||await context.newPage();
    await page.goto('data:text/html,%%3Ch1%%20id=%%22ready%%22%%3Eone%%3C/h1%%3E');
    if (await page.locator('#ready').textContent() !== 'one') throw Error('own page unavailable');
    await context.tracing.start({screenshots:true,snapshots:true});
    await page.locator('#ready').evaluate(el=>el.textContent='two');
    const image=await page.screenshot({type:'jpeg'});
    await context.tracing.stop({path:%q});
    if (image.length < 100 || await page.locator('#ready').textContent() !== 'two')
      throw Error('same-session Playwright/visual state unavailable');
    process.stdout.write('A_OWN_OK_AND_PEER_WRITE_DENIED\n');
  } finally {await context.close();}
}`,
		filepath.Join(ownA, "synthetic-cookie.txt"), peerState, ownB, peerState, filepath.Join(ownA, "synthetic-cookie.txt"), peerState, filepath.Join(ownA, "synthetic-cookie.txt"),
		filepath.Join(engine, "browsers"),
		"file://"+filepath.ToSlash(filepath.Join(engine, "node_modules", "playwright", "index.mjs")),
		ownA, filepath.Join(scratch, traceA, "trace.zip"))
	a := run(ctx, profileA, traceA, aScript)
	out, err := a.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "A_OWN_OK_AND_PEER_WRITE_DENIED") {
		t.Fatalf("A could not write its profile or could mutate peer profile (%v): %s", err, out)
	}
	content, err := os.ReadFile(peerState)
	if err != nil || string(content) != "B-ORIGINAL" {
		t.Fatalf("peer Browser data corrupted: %v %q", err, content)
	}
	if _, err := bIn.Write([]byte("verify\n")); err != nil {
		t.Fatal(err)
	}
	verified, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(verified) != "B_STATE=B-ORIGINAL" {
		t.Fatalf("peer session did not survive concurrently: %v %q %s", err, verified, bErr.String())
	}
}
