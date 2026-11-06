package managedincus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const fakeIncus = `#!/usr/bin/python3
import sys,json,os
p = %q
args = sys.argv[1:]
if args and args[0] == '--force-local': args = args[1:]
state = json.load(open(p)) if os.path.exists(p) else {'instances':[],'snapshots':{}}
def save():
    with open(p+'.tmp','w') as f: json.dump(state,f)
    os.replace(p+'.tmp',p)
def value(option,default=''):
    return args[args.index(option)+1] if option in args else default
if args[0]=='info':
    print('{}')
elif args[0]=='list':
    project=value('--project')
    result=state['instances']
    if project: result=[x for x in result if x['project']==project]
    print(json.dumps(result))
elif args[0]=='launch':
    image=args[1];name=args[2];project=value('--project','default')
    cfg={}
    for i,x in enumerate(args[:-1]):
        if x=='-c':
            key,v=args[i+1].split('=',1);cfg[key]=v
    inst={'name':name,'project':project,'type':'virtual-machine' if '--vm' in args else 'container','status':'Running','config':cfg|{'volatile.uuid':'12345678-1234-1234-1234-123456789abc'}}
    state['instances'].append(inst);save()
    print('created')
elif args[0]=='snapshot' and args[1]=='list':
    k=value('--project','default')+':'+args[2]
    print(json.dumps([{'name':name} for name in state['snapshots'].get(k,[])]))
elif args[0]=='snapshot' and args[1]=='create':
    k=value('--project','default')+':'+args[2]
    state['snapshots'].setdefault(k,[]).append(args[3]);save()
elif args[0]=='snapshot' and args[1]=='restore':
    k=value('--project','default')+':'+args[2]
    if args[3] not in state['snapshots'].get(k,[]):sys.exit(41)
    save()
elif args[0] in ('start','stop','delete'):
    name=args[1];project=value('--project','default')
    found=[x for x in state['instances'] if x['name']==name and x['project']==project]
    if not found:sys.exit(42)
    if args[0]=='delete':state['instances'].remove(found[0])
    else: found[0]['status']='Running' if args[0]=='start' else 'Stopped'
    save()
elif args[0]=='exec':
    # Deliberately reflect stdin into stdout to prove the Agent discards it
    # when a secret reference is supplied.
    if '--' not in args:sys.exit(43)
    print('native-exec-ok')
    content=sys.stdin.read()
    if content:print(content)
else: sys.exit(44)
`

func fixture(t *testing.T) Backend {
	t.Helper()
	d := t.TempDir()
	cli := filepath.Join(d, "incus-mock")
	data := fmt.Sprintf(fakeIncus, filepath.Join(d, "native.json"))
	if err := os.WriteFile(cli, []byte(data), 0700); err != nil {
		t.Fatal(err)
	}
	return Backend{HostID: "asa_one_host", Binary: cli, StateDir: d, capacityProbe: func(int, int) error { return nil }}
}
func createRequest() Request {
	return Request{Operation: "create", Project: "default", Instance: "guest-a", Image: "images:debian/13", Type: "container", WorkID: "issue65", OperationID: "stable-creation-id", CPU: 1, MemoryMiB: 128, DiskGiB: 1}
}
func TestNativeLifecycleIdentityCollisionAndRecovery(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	ready := b.Ready(ctx)
	if !ready.Available {
		t.Fatalf("fake optional daemon readiness: %+v", ready)
	}
	req := createRequest()
	created, err := b.Run(ctx, req, false, "")
	if err != nil || created.Instance == nil || created.Status != "created" {
		t.Fatalf("create: %+v %v", created, err)
	}
	uuid := created.Instance.UUID
	same, err := b.Run(ctx, req, false, "")
	if err != nil || same.Status != "reconciled_existing" || same.Instance.UUID != uuid {
		t.Fatalf("idempotent create: %+v %v", same, err)
	}
	conflict := req
	conflict.OperationID = "different-op"
	if _, err := b.Run(ctx, conflict, false, ""); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("different create operation reused existing guest: %v", err)
	}
	other := req
	other.Instance = "guest-b"
	other.OperationID = "second"
	if _, err := b.Run(ctx, other, false, ""); err != nil {
		t.Fatal(err)
	}
	all, err := b.List(ctx, "default")
	if err != nil || len(all) != 2 {
		t.Fatalf("multiple guests missing: %+v %v", all, err)
	}
	b2 := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe}
	recovered, err := b2.Run(ctx, Request{Operation: "inspect", Project: "default", Instance: req.Instance}, false, "")
	if err != nil || recovered.Instance.UUID != uuid {
		t.Fatalf("cold rediscovery: %+v %v", recovered, err)
	}
	mutated := Request{Operation: "stop", Project: "default", Instance: req.Instance, UUID: uuid}
	if _, err := b.Run(ctx, Request{Operation: "start", Project: "default", Instance: req.Instance, UUID: "wrong-uuid"}, false, ""); err == nil {
		t.Fatal("guest identity alias authorized")
	}
	if _, err := b.Run(ctx, mutated, false, ""); err != nil {
		t.Fatal(err)
	}
	deleted, err := b.Run(ctx, Request{Operation: "delete", Project: "default", Instance: req.Instance, UUID: uuid}, true, "")
	if err != nil || deleted.Status != "deleted" {
		t.Fatalf("delete: %+v %v", deleted, err)
	}
	if _, err := b.Run(ctx, mutated, false, ""); err == nil {
		t.Fatal("missing guest mutation authorized")
	}
}
func TestSnapshotRestoreAndCredentialStdinTransport(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	id := created.Instance.UUID
	ref := Request{Project: "default", Instance: "guest-a", UUID: id}
	sn := ref
	sn.Operation = "snapshot"
	sn.Snapshot = "before-test"
	result, err := b.Run(ctx, sn, false, "")
	if err != nil || result.Status != "snapshot_created" {
		t.Fatalf("snapshot: %+v %v", result, err)
	}
	snaps := ref
	snaps.Operation = "snapshots"
	observed, err := b.Run(ctx, snaps, false, "")
	if err != nil || len(observed.Snapshots) != 1 {
		t.Fatalf("snapshot list: %+v %v", observed, err)
	}
	restore := ref
	restore.Operation = "restore"
	restore.Snapshot = "before-test"
	if _, err := b.Run(ctx, restore, false, ""); err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatal("restore requires approval")
	}
	if _, err := b.Run(ctx, restore, true, ""); err != nil {
		t.Fatalf("mock native restore: %v", err)
	}
	ex := ref
	ex.Operation = "exec"
	ex.Command = []string{"sh", "-c", "read -r password; true"}
	regular, err := b.Run(ctx, ex, false, "")
	if err != nil || !strings.Contains(regular.Output, "native-exec-ok") {
		t.Fatalf("regular guest exec: %+v %v", regular, err)
	}
	secret := "TEST-SECRET-NEVER-ECHO"
	private, err := b.Run(ctx, ex, false, secret)
	if err != nil || private.Output != "" || private.Status != "guest_command_completed" {
		t.Fatalf("secret-bearing guest exec stdout leak: %+v %v", private, err)
	}
}
func TestConcurrentCreateSameOperationSingleInstance(t *testing.T) {
	b := fixture(t)
	var wg sync.WaitGroup
	errors := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.Run(context.Background(), createRequest(), false, "")
			if err != nil {
				errors <- err
			} else if r.Instance == nil {
				errors <- fmt.Errorf("no verified instance")
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	all, err := b.List(context.Background(), "default")
	if err != nil || len(all) != 1 {
		t.Fatalf("concurrent create duplicate: %+v %v", all, err)
	}
}
func TestInvalidInputFailsBeforeMutation(t *testing.T) {
	b := fixture(t)
	for _, r := range []Request{
		{Operation: "create", Project: "default", Instance: "guest-a", Type: "container", Image: "bad;rm", WorkID: "issue65", OperationID: "id", CPU: 1, MemoryMiB: 128, DiskGiB: 1},
		{Operation: "create", Project: "default", Instance: "../guest", Type: "container", Image: "images:debian/13", WorkID: "issue65", OperationID: "id", CPU: 1, MemoryMiB: 128, DiskGiB: 1},
		{Operation: "create", Project: "default", Instance: "guest-a", Type: "container", Image: "images:debian/13", WorkID: "issue65", OperationID: "id", CPU: 0, MemoryMiB: 128, DiskGiB: 1},
	} {
		if _, err := b.Run(context.Background(), r, false, ""); err == nil {
			t.Fatalf("invalid create accepted: %+v", r)
		}
	}
}
