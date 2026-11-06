package managedincus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeIncus = `#!/usr/bin/python3
import sys,json,os,time
p = %q
args = sys.argv[1:]
if args and args[0] == '--force-local': args = args[1:]
state = json.load(open(p)) if os.path.exists(p) else {'instances':[],'snapshots':{}}
mode = open(p+'.mode').read().strip() if os.path.exists(p+'.mode') else ''
def save():
    with open(p+'.tmp','w') as f: json.dump(state,f)
    os.replace(p+'.tmp',p)
def value(option,default=''):
    return args[args.index(option)+1] if option in args else default
if args[0]=='info':
    if '--format' in args:sys.exit(65)
    print('{}')
elif args[0]=='query' and args[1]=='/1.0':
    if mode=='api_unavailable':sys.exit(46)
    if mode=='api_bad_json':print('{bad-json');sys.exit(0)
    if mode=='api_missing_version':print('{}');sys.exit(0)
    print(json.dumps({'api_version':'1.0'}))
elif args[0]=='query' and '/1.0/storage-pools/' in args[1]:
    if mode=='pool_unavailable':sys.exit(43)
    total=16*(1<<30)
    used=total if mode=='pool_full' else 2*(1<<30)
    print(json.dumps({'space':{'total':total,'used':used}}))
elif args[0]=='operation' and args[1]=='list':
    if mode=='operation_unavailable':sys.exit(49)
    if mode=='operation_malformed':print('broken-json');sys.exit(0)
    if mode=='pending_second_scan':
        state['operation_list_calls']=state.get('operation_list_calls',0)+1;save()
        if state['operation_list_calls']>=2:
            print(json.dumps([{'id':'ddccbb11-1234-1234-1234-123456789abc','status':'Running',
              'description':'Creating instance','resources':{'instances':['/1.0/instances/guest-a']}}]))
            sys.exit(0)
    if mode=='pending_operation':
        print(json.dumps([{'id':'ddccbb11-1234-1234-1234-123456789abc','status':'Running',
            'description':'Creating instance','resources':{'instances':['/1.0/instances/guest-a']}}]))
        sys.exit(0)
    if mode=='late_native_visible':
        state['instances'].append({'name':'guest-a','project':'default','type':'container',
          'status':'Running','config':{'user.asa.host':'asa_one_host','user.asa.work':'issue65',
          'user.asa.operation':'stable-creation-id','volatile.uuid':'12345678-1234-1234-1234-123456789abc'},
          'devices':{'root':{'pool':'default','size':'1GiB'}}});save()
    print('[]')
elif args[0]=='list':
    if mode=='list_unavailable':sys.exit(48)
    if '--all-projects' in args:
        if mode=='list_null':print('null');sys.exit(0)
        if mode=='list_null_row':print('[null]');sys.exit(0)
        if mode=='list_missing_project':
            print(json.dumps([{'name':'guest-other','type':'container'}]));sys.exit(0)
        if mode=='list_bad_identity':
            print(json.dumps([{'name':'guest/other','project':'other','type':'container'}]));sys.exit(0)
        if mode=='list_bad_project':
            print(json.dumps([{'name':'guest-other','project':'wrong/project','type':'container'}]));sys.exit(0)
        if mode=='list_bad_name_type':
            print(json.dumps([{'name':404,'project':'default','type':'container'}]));sys.exit(0)
        if mode=='list_valid_unrelated':
            print(json.dumps([{'name':'different-guest','project':'different-project','type':'container'}]));sys.exit(0)
        if mode=='list_valid_empty':
            print('[]');sys.exit(0)
    project=value('--project')
    result=state['instances']
    if project: result=[x for x in result if x['project']==project]
    print(json.dumps(result))
elif args[0]=='launch':
    if mode=='sleep_launch':time.sleep(10)
    if mode=='uncertain_launch':sys.exit(45)
    image=args[1];name=args[2];project=value('--project','default')
    cfg={}
    for i,x in enumerate(args[:-1]):
        if x=='-c':
            key,v=args[i+1].split('=',1);cfg[key]=v
    uid='12345678-1234-1234-1234-123456789abc'
    if mode=='missing_uuid':uid=''
    if mode=='bad_uuid':uid='not-a-valid-uuid'
    devices={'root':{'type':'disk','pool':value('--storage','default'),'size':'1GiB'}}
    inst={'name':name,'project':project,'type':'virtual-machine' if '--vm' in args else 'container','status':'Running','devices':devices,'expanded_devices':devices,'config':cfg|{'volatile.uuid':uid}}
    state['instances'].append(inst);save()
    print('created')
elif args[0]=='snapshot' and args[1]=='list':
    k=value('--project','default')+':'+args[2]
    print(json.dumps([{'name':name} for name in state['snapshots'].get(k,[])]))
elif args[0]=='snapshot' and args[1]=='create':
    k=value('--project','default')+':'+args[2]
    state['snapshots'].setdefault(k,[]).append(args[3]);save()
    if mode=='snapshot_lost_ack':sys.exit(45)
elif args[0]=='snapshot' and args[1]=='restore':
    k=value('--project','default')+':'+args[2]
    if args[3] not in state['snapshots'].get(k,[]):sys.exit(41)
    save()
    if mode=='restore_lost_ack':sys.exit(45)
elif args[0] in ('start','stop','delete'):
    name=args[1];project=value('--project','default')
    found=[x for x in state['instances'] if x['name']==name and x['project']==project]
    if not found:sys.exit(42)
    if args[0]=='delete':state['instances'].remove(found[0])
    else:
        found[0]['status']='Running' if args[0]=='start' else 'Stopped'
        if mode=='change_operation_during_stop' and args[0]=='stop':
            found[0]['config'].pop('user.asa.operation',None)
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
func TestNativeReadinessQueriesSupportedAPI(t *testing.T) {
	b := fixture(t)
	ready := b.Ready(context.Background())
	if !ready.Available || ready.Status != "ready" {
		t.Fatalf("healthy native API rejected: %+v", ready)
	}
	for _, mode := range []string{"api_unavailable", "api_bad_json", "api_missing_version"} {
		t.Run(mode, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			got := b.Ready(context.Background())
			if got.Available || got.Status != "unavailable" || !strings.HasPrefix(got.Diagnostic, "incus_unready:") {
				t.Fatalf("unavailable or malformed native API treated as ready: %+v", got)
			}
		})
	}
}

func createRequest() Request {
	return Request{Operation: "create", Project: "default", Instance: "guest-a", Image: "images:debian/13", Type: "container", WorkID: "issue65", OperationID: "stable-creation-id", CPU: 1, MemoryMiB: 128, DiskGiB: 1, StoragePool: "default"}
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
	sameOperationNewName := req
	sameOperationNewName.Instance = "guest-b"
	if _, err := b.Run(ctx, sameOperationNewName, false, ""); err == nil || !strings.Contains(err.Error(), "operation_id_collision") {
		t.Fatalf("same create operation duplicated under new name: %v", err)
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
func TestSnapshotRestoreLostAcknowledgementNeverClaimsCompletion(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	created, err := b.Run(ctx, createRequest(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := Request{Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "before-test"}
	writeMode := func(mode string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte(mode), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeMode("snapshot_lost_ack")
	snapshot := ref
	snapshot.Operation = "snapshot"
	if result, err := b.Run(ctx, snapshot, false, ""); err == nil || !strings.Contains(err.Error(), "snapshot_outcome_uncertain") || result.Status == "snapshot_created" {
		t.Fatalf("lost snapshot acknowledgement treated as completion: %+v %v", result, err)
	}
	// A native snapshot may already be visible while its daemon operation
	// is still running. The Agent must not silently submit it again.
	observed, err := b.Snapshots(ctx, ref.Project, ref.Instance)
	if err != nil || !hasSnapshot(observed, ref.Snapshot) {
		t.Fatalf("native snapshot state not discoverable: %v %v", observed, err)
	}
	if _, err := b.Run(ctx, snapshot, false, ""); err == nil || !strings.Contains(err.Error(), "snapshot_collision") {
		t.Fatalf("uncertain native snapshot was submitted twice: %v", err)
	}
	writeMode("restore_lost_ack")
	restore := ref
	restore.Operation = "restore"
	if result, err := b.Run(ctx, restore, true, ""); err == nil || !strings.Contains(err.Error(), "restore_outcome_uncertain") || result.Status == "completed_native" {
		t.Fatalf("lost restore acknowledgement treated as completion: %+v %v", result, err)
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
		{Operation: "create", Project: "default", Instance: "guest-a", Type: "container", Image: "bad;rm", WorkID: "issue65", OperationID: "id", CPU: 1, MemoryMiB: 128, DiskGiB: 1, StoragePool: "default"},
		{Operation: "create", Project: "default", Instance: "../guest", Type: "container", Image: "images:debian/13", WorkID: "issue65", OperationID: "id", CPU: 1, MemoryMiB: 128, DiskGiB: 1, StoragePool: "default"},
		{Operation: "create", Project: "default", Instance: "guest-a", Type: "container", Image: "images:debian/13", WorkID: "issue65", OperationID: "id", CPU: 0, MemoryMiB: 128, DiskGiB: 1, StoragePool: "default"},
	} {
		if _, err := b.Run(context.Background(), r, false, ""); err == nil {
			t.Fatalf("invalid create accepted: %+v", r)
		}
	}
}

func TestDurableUncertainCreateReconnectAndNameCollision(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	native := filepath.Join(b.StateDir, "native.json")
	if err := os.WriteFile(native+".mode", []byte("uncertain_launch"), 0600); err != nil {
		t.Fatal(err)
	}
	req := createRequest()
	_, err := b.Run(ctx, req, false, "")
	if err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") {
		t.Fatalf("ambiguous create was treated as definite failure: %v", err)
	}
	if _, err := os.Stat(b.intentPath()); err != nil {
		t.Fatalf("missing durable intent before launch: %v", err)
	}
	pending, err := b.Run(ctx, Request{Operation: "pending_creates"}, false, "")
	if err != nil || len(pending.Pending) != 1 || pending.Pending[0].State != "submitted" {
		t.Fatalf("pending recovery metadata unavailable: %+v %v", pending, err)
	}
	restarted := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe}
	if _, err := restarted.Run(ctx, req, false, ""); err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") {
		t.Fatalf("restart duplicated pending create: %v", err)
	}
	changed := req
	changed.Instance = "other-guest"
	if _, err := restarted.Run(ctx, changed, false, ""); err == nil || !strings.Contains(err.Error(), "create_intent_collision") {
		t.Fatalf("changed-name retry duplicated operation: %v", err)
	}
	changed = req
	changed.OperationID = "different-key"
	if _, err := restarted.Run(ctx, changed, false, ""); err == nil || !strings.Contains(err.Error(), "create_intent_collision") {
		t.Fatalf("changed-operation retry with same name duplicated: %v", err)
	}
	if err := os.Remove(native + ".mode"); err != nil {
		t.Fatal(err)
	}
	// Simulate a prior Incus asynchronous operation becoming visible after
	// the original process returned. New Agent only inspects and reconciles.
	_, err = restarted.command(ctx, 5*time.Second, "launch", req.Image, req.Instance, "--project", req.Project, "--storage", req.StoragePool,
		"-c", "user.asa.host="+b.HostID, "-c", "user.asa.work="+req.WorkID, "-c", "user.asa.operation="+req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := restarted.Run(ctx, req, false, "")
	if err != nil || observed.Status != "reconciled_existing" || observed.Instance == nil {
		t.Fatalf("late completion not recovered: %+v %v", observed, err)
	}
	intents, err := restarted.loadIntents()
	if err != nil || len(intents.Intents) != 1 || intents.Intents[0].State != "confirmed" {
		t.Fatalf("intent not reconciled: %+v %v", intents, err)
	}
}

func TestNativeInvalidUUIDRefusesCreate(t *testing.T) {
	for _, mode := range []string{"missing_uuid", "bad_uuid"} {
		t.Run(mode, func(t *testing.T) {
			b := fixture(t)
			if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			req := createRequest()
			if result, err := b.Run(context.Background(), req, false, ""); err == nil || !strings.Contains(err.Error(), "native_uuid_unverified") {
				t.Fatalf("invalid UUID reported created: %+v %v", result, err)
			}
			if _, err := b.Run(context.Background(), Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: "12345678-1234-1234-1234-123456789abc"}, false, ""); err == nil {
				t.Fatal("malformed native identity authorized")
			}
		})
	}
}

func TestNativePoolCapacityAndEffectiveDisk(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	b.capacityProbe = nil
	if err := b.checkNativePool(ctx, "default", 1); err != nil {
		t.Fatalf("native pool unexpectedly insufficient: %v", err)
	}
	if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte("pool_full"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.checkNativePool(ctx, "default", 1); err == nil || !strings.Contains(err.Error(), "incus_storage_capacity_insufficient") {
		t.Fatalf("false storage preflight: %v", err)
	}
	if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte("pool_unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.checkNativePool(ctx, "default", 1); err == nil || !strings.Contains(err.Error(), "incus_storage_unavailable") {
		t.Fatalf("unavailable native pool treated as ready: %v", err)
	}
	if err := os.Remove(filepath.Join(b.StateDir, "native.json.mode")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, createRequest(), false, ""); err != nil {
		t.Fatal(err)
	}
	all, err := b.List(ctx, "default")
	if err != nil || len(all) != 1 || all[0].StoragePool != "default" || all[0].DiskLimit != "1GiB" {
		t.Fatalf("root disk limits not visible: %+v %v", all, err)
	}
}

func TestMutationsRejectIdentityReplacementAtFinalCheck(t *testing.T) {
	for _, op := range []string{"start", "stop", "exec", "snapshot", "restore", "delete"} {
		t.Run(op, func(t *testing.T) {
			b := fixture(t)
			ctx := context.Background()
			created, err := b.Run(ctx, createRequest(), false, "")
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Operation: op, Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Command: []string{"true"}, Snapshot: "snap"}
			if op == "delete" {
				if _, err := b.Run(ctx, Request{Operation: "stop", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID}, false, ""); err != nil {
					t.Fatal(err)
				}
			}
			if op == "snapshot" || op == "restore" {
				if _, err := b.Run(ctx, Request{Operation: "snapshot", Project: "default", Instance: "guest-a", UUID: created.Instance.UUID, Snapshot: "snap"}, false, ""); err != nil {
					t.Fatal(err)
				}
				if op == "snapshot" {
					req.Snapshot = "second"
				}
			}
			b.beforeMutation = func(Request) {
				path := filepath.Join(b.StateDir, "native.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var native map[string]any
				if err := json.Unmarshal(data, &native); err != nil {
					t.Fatal(err)
				}
				instances := native["instances"].([]any)
				entry := instances[0].(map[string]any)
				entry["config"].(map[string]any)["volatile.uuid"] = "99887766-1234-1234-1234-123456789abc"
				data, err = json.Marshal(native)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := req.Operation
			result, err := b.Run(ctx, req, true, "")
			if err == nil || !strings.Contains(err.Error(), "identity_changed_before_mutation") {
				t.Fatalf("%s acted on replacement: %+v %v", before, result, err)
			}
			native, err := b.List(ctx, "default")
			if err != nil || len(native) != 1 || native[0].UUID != "99887766-1234-1234-1234-123456789abc" {
				t.Fatalf("replacement changed by Agent: %+v %v", native, err)
			}
		})
	}
}

func TestInterruptedNativeCreateKeepsIntentAfterCancellation(t *testing.T) {
	b := fixture(t)
	modePath := filepath.Join(b.StateDir, "native.json.mode")
	if err := os.WriteFile(modePath, []byte("sleep_launch"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := createRequest()
	_, err := b.Run(ctx, req, false, "")
	if err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") {
		t.Fatalf("interrupt returned a definitive outcome: %v", err)
	}
	other := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe}
	status, err := other.Run(context.Background(), Request{Operation: "pending_creates"}, false, "")
	if err != nil || len(status.Pending) != 1 || status.Pending[0].OperationID != req.OperationID {
		t.Fatalf("interrupted create missing after recovery: %+v %v", status, err)
	}
	if _, err := other.Run(context.Background(), req, false, ""); err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") {
		t.Fatalf("reconnected Agent silently relaunched interrupted operation: %v", err)
	}
}

func TestConfirmedCreateUUIDSurvivesReconnectAndRejectsTagClonedReplacement(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	req := createRequest()
	first, err := b.Run(ctx, req, false, "")
	if err != nil || first.Instance == nil {
		t.Fatalf("initial create failed: %+v %v", first, err)
	}
	nativeUUID := first.Instance.UUID
	d, err := b.loadIntents()
	if err != nil || len(d.Intents) != 1 || d.Intents[0].NativeUUID != nativeUUID || d.Intents[0].State != "confirmed" {
		t.Fatalf("confirmed UUID not persisted: %+v %v", d, err)
	}
	restarted := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe}
	if out, err := restarted.Run(ctx, req, false, ""); err != nil || out.Status != "reconciled_existing" {
		t.Fatalf("original UUID not reconciled: %+v %v", out, err)
	}
	path := filepath.Join(b.StateDir, "native.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]any
	if err := json.Unmarshal(raw, &native); err != nil {
		t.Fatal(err)
	}
	entry := native["instances"].([]any)[0].(map[string]any)
	// Every identifying project/name/host/work/operation tag remains copied.
	entry["config"].(map[string]any)["volatile.uuid"] = "99887766-1234-1234-1234-123456789abc"
	raw, err = json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := restarted.Run(ctx, req, false, ""); err == nil || !strings.Contains(err.Error(), "create_intent_native_uuid_conflict") {
		t.Fatalf("copied tags authorized replacement as original: %+v %v", out, err)
	}
	if _, err := restarted.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: "99887766-1234-1234-1234-123456789abc"}, false, ""); err == nil || !strings.Contains(err.Error(), "create_intent_native_uuid_conflict") {
		t.Fatalf("replacement accepted for guest mutation: %v", err)
	}
	d, err = restarted.loadIntents()
	if err != nil || d.Intents[0].NativeUUID != nativeUUID {
		t.Fatalf("confirmed UUID was overwritten by a replacement: %+v %v", d, err)
	}
}

func TestApprovedAbandonRequiresProvenNativeAbsenceAndRetainsTombstone(t *testing.T) {
	b := fixture(t)
	req := createRequest()
	ctx := context.Background()
	modePath := filepath.Join(b.StateDir, "native.json.mode")
	if err := os.WriteFile(modePath, []byte("uncertain_launch"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, req, false, ""); err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") {
		t.Fatalf("setup missing uncertain create: %v", err)
	}
	abandon := Request{Operation: "abandon_create", Project: req.Project, Instance: req.Instance, OperationID: req.OperationID, WorkID: req.WorkID}
	if _, err := b.Run(ctx, abandon, false, ""); err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatalf("abandon without approval accepted: %v", err)
	}
	wrong := abandon
	wrong.WorkID = "another-work"
	if _, err := b.Run(ctx, wrong, true, ""); err == nil || !strings.Contains(err.Error(), "identity_mismatch") {
		t.Fatalf("wrong-scoped abandonment accepted: %v", err)
	}
	b2 := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe}
	for _, mode := range []string{"pending_operation", "pending_second_scan", "operation_unavailable", "operation_malformed", "list_unavailable", "late_native_visible"} {
		if err := os.WriteFile(modePath, []byte(mode), 0600); err != nil {
			t.Fatal(err)
		}
		if mode == "pending_second_scan" {
			// Native fixture starts with an empty first operation scan.
			// In a second scan it makes the pending background job visible.
			if err := os.Remove(filepath.Join(b.StateDir, "native.json")); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		if _, err := b2.Run(ctx, abandon, true, ""); err == nil || !(strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), "identity_conflict")) {
			t.Fatalf("ambiguous native state (%s) allowed abandonment: %v", mode, err)
		}
		data, err := b2.loadIntents()
		if err != nil || len(data.Intents) != 1 || data.Intents[0].State != "submitted" {
			t.Fatalf("ambiguous mode (%s) erased pending intent: %+v %v", mode, data, err)
		}
		if mode == "late_native_visible" {
			// Preserve injected native guest for checking: after operations become
			// empty, an existing name must still prevent the retirement.
			if err := os.Remove(modePath); err != nil {
				t.Fatal(err)
			}
			if _, err := b2.Run(ctx, abandon, true, ""); err == nil || !strings.Contains(err.Error(), "identity_conflict") {
				t.Fatalf("native instance after pending launch not detected: %v", err)
			}
			// Test-only native fixture disappearance: independent owner-verified
			// negative state after external removal (not a production cleanup).
			if err := os.Remove(filepath.Join(b.StateDir, "native.json")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(modePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out, err := b2.Run(ctx, abandon, true, "")
	if err != nil || out.Status != "create_intent_abandoned" || len(out.Pending) != 1 || out.Pending[0].State != "abandoned" {
		t.Fatalf("approved verified abandonment blocked: %+v %v", out, err)
	}
	record, err := b2.loadIntents()
	if err != nil || len(record.Intents) != 1 || record.Intents[0].State != "abandoned" || record.Intents[0].AbandonedAt == nil {
		t.Fatalf("durable tombstone missing: %+v %v", record, err)
	}
	if _, err := b2.Run(ctx, req, false, ""); err == nil || !strings.Contains(err.Error(), "abandoned_operation_id") {
		t.Fatalf("retired operation ID allowed relaunch: %v", err)
	}
	newer := req
	newer.OperationID = "fresh-attempt-after-abandon"
	newer.Image = "images:ubuntu/24.04"
	created, err := b2.Run(ctx, newer, false, "")
	if err != nil || created.Status != "created" {
		t.Fatalf("new attempt after verified abandoned identity failed: %+v %v", created, err)
	}
	current, err := b2.List(ctx, "default")
	if err != nil || len(current) != 1 {
		t.Fatalf("abandon/recovery duplicate native resources: %+v %v", current, err)
	}
	uuid := created.Instance.UUID
	if _, err := b2.Run(ctx, Request{Operation: "stop", Project: newer.Project, Instance: newer.Instance, UUID: uuid}, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Run(ctx, Request{Operation: "delete", Project: newer.Project, Instance: newer.Instance, UUID: uuid}, true, ""); err != nil {
		t.Fatal(err)
	}
	journal, err := b2.loadIntents()
	if err != nil || len(journal.Intents) != 1 || journal.Intents[0].OperationID != req.OperationID || journal.Intents[0].State != "abandoned" {
		t.Fatalf("deleting fresh guest also removed retired old operation ID: %+v %v", journal, err)
	}
	if _, err := b2.Run(ctx, req, false, ""); err == nil || !strings.Contains(err.Error(), "abandoned_operation_id") {
		t.Fatalf("old operation ID replayed after deleting fresh guest: %v", err)
	}
}

func TestCannotAbandonConfirmedCreate(t *testing.T) {
	b := fixture(t)
	req := createRequest()
	ctx := context.Background()
	if _, err := b.Run(ctx, req, false, ""); err != nil {
		t.Fatal(err)
	}
	abandon := Request{Operation: "abandon_create", Project: req.Project, Instance: req.Instance, WorkID: req.WorkID, OperationID: req.OperationID}
	if _, err := b.Run(ctx, abandon, true, ""); err == nil || !strings.Contains(err.Error(), "not_pending") {
		t.Fatalf("confirmed resource was abandoned: %v", err)
	}
}

func TestMissingConfirmedUUIDNeverSilentlyRebinds(t *testing.T) {
	b := fixture(t)
	req := createRequest()
	out, err := b.Run(context.Background(), req, false, "")
	if err != nil || out.Instance == nil {
		t.Fatalf("create: %+v %v", out, err)
	}
	d, err := b.loadIntents()
	if err != nil {
		t.Fatal(err)
	}
	d.Intents[0].NativeUUID = "" // simulate a historical/partially-written confirmation
	if err := b.saveIntents(d); err != nil {
		t.Fatal(err)
	}
	restarted := Backend{HostID: b.HostID, Binary: b.Binary, StateDir: b.StateDir, capacityProbe: b.capacityProbe}
	if out, err := restarted.Run(context.Background(), req, false, ""); err == nil || !strings.Contains(err.Error(), "create_intent_uuid_unavailable") {
		t.Fatalf("missing original UUID silently rebound: %+v %v", out, err)
	}
	if _, err := restarted.Run(context.Background(), Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: out.Instance.UUID}, false, ""); err == nil || !strings.Contains(err.Error(), "create_intent_uuid_unavailable") {
		t.Fatalf("missing immutable UUID allowed guest mutation: %v", err)
	}
}

func TestAbandonRejectsIncompleteSuccessfulNativeAllProjectInventory(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		allowed bool
	}{
		{"list_null", false},
		{"list_null_row", false},
		{"list_missing_project", false},
		{"list_bad_identity", false},
		{"list_bad_project", false},
		{"list_bad_name_type", false},
		{"list_valid_empty", true},
		{"list_valid_unrelated", true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			b := fixture(t)
			ctx := context.Background()
			native := filepath.Join(b.StateDir, "native.json")
			if err := os.WriteFile(native+".mode", []byte("uncertain_launch"), 0600); err != nil {
				t.Fatal(err)
			}
			req := createRequest()
			if _, err := b.Run(ctx, req, false, ""); err == nil || !strings.Contains(err.Error(), "create_outcome_uncertain") {
				t.Fatalf("failed native launch setup: %v", err)
			}
			if err := os.WriteFile(native+".mode", []byte(tc.mode), 0600); err != nil {
				t.Fatal(err)
			}
			abandon := Request{Operation: "abandon_create", Project: req.Project, Instance: req.Instance, OperationID: req.OperationID, WorkID: req.WorkID}
			result, err := b.Run(ctx, abandon, true, "")
			journal, loadErr := b.loadIntents()
			if loadErr != nil || len(journal.Intents) != 1 {
				t.Fatalf("journal unavailable: %+v %v", journal, loadErr)
			}
			if tc.allowed {
				if err != nil || result.Status != "create_intent_abandoned" || journal.Intents[0].State != "abandoned" {
					t.Fatalf("valid native inventory refused: result=%+v err=%v journal=%+v", result, err, journal)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "create_abandon_outcome_uncertain") {
					t.Fatalf("incomplete successful native inventory accepted: result=%+v err=%v", result, err)
				}
				if journal.Intents[0].State != "submitted" {
					t.Fatalf("ambiguous inventory erased pending intent: %+v", journal)
				}
			}
		})
	}
}

func TestJournalBoundNativeIdentityRefusesReplacedGuestWithChangedTags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replaceUUID bool
		operation   string
		work        string
		want        string
	}{
		{"same_tags_replaced_uuid", true, "unchanged", "unchanged", "create_intent_native_uuid_conflict"},
		{"changed_operation_replaced_uuid", true, "changed", "unchanged", "create_intent_native_uuid_conflict"},
		{"missing_operation_replaced_uuid", true, "missing", "unchanged", "create_intent_native_uuid_conflict"},
		{"changed_work_replaced_uuid", true, "unchanged", "changed", "create_intent_native_uuid_conflict"},
		{"changed_operation_same_uuid", false, "changed", "unchanged", "create_intent_identity_mismatch"},
		{"missing_operation_same_uuid", false, "missing", "unchanged", "create_intent_identity_mismatch"},
		{"changed_work_same_uuid", false, "unchanged", "changed", "create_intent_identity_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fixture(t)
			ctx := context.Background()
			req := createRequest()
			original, err := b.Run(ctx, req, false, "")
			if err != nil || original.Instance == nil {
				t.Fatalf("create: %+v %v", original, err)
			}
			uuid := original.Instance.UUID
			nativeFile := filepath.Join(b.StateDir, "native.json")
			content, err := os.ReadFile(nativeFile)
			if err != nil {
				t.Fatal(err)
			}
			var native map[string]any
			if err := json.Unmarshal(content, &native); err != nil {
				t.Fatal(err)
			}
			entry := native["instances"].([]any)[0].(map[string]any)
			cfg := entry["config"].(map[string]any)
			if tc.replaceUUID {
				uuid = "99887766-1234-1234-1234-123456789abc"
				cfg["volatile.uuid"] = uuid
			}
			if tc.operation == "changed" {
				cfg["user.asa.operation"] = "new-operation"
			}
			if tc.operation == "missing" {
				delete(cfg, "user.asa.operation")
			}
			if tc.work == "changed" {
				cfg["user.asa.work"] = "new-work"
			}
			// Native host ownership, project and name remain identical.
			bytes, err := json.Marshal(native)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(nativeFile, bytes, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := b.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: uuid}, false, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("journal identity bypass allowed: result=%+v err=%v", result, err)
			}
			all, err := b.List(ctx, req.Project)
			if err != nil || len(all) != 1 || all[0].Status != "Running" {
				t.Fatalf("replacement guest mutated during refused action: %+v %v", all, err)
			}
		})
	}
}

func TestFreshConfirmedNameReuseAfterVerifiedDeletion(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	req := createRequest()
	first, err := b.Run(ctx, req, false, "")
	if err != nil || first.Instance == nil {
		t.Fatalf("first create: %+v %v", first, err)
	}
	uuid := first.Instance.UUID
	if _, err := b.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: uuid}, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(ctx, Request{Operation: "delete", Project: req.Project, Instance: req.Instance, UUID: uuid}, true, ""); err != nil {
		t.Fatal(err)
	}
	newer := req
	newer.OperationID = "new-after-delete"
	fresh, err := b.Run(ctx, newer, false, "")
	if err != nil || fresh.Status != "created" {
		t.Fatalf("legitimate new create refused: %+v %v", fresh, err)
	}
	if _, err := b.Run(ctx, Request{Operation: "stop", Project: newer.Project, Instance: newer.Instance, UUID: fresh.Instance.UUID}, false, ""); err != nil {
		t.Fatalf("fresh confirmed guest mutation refused: %v", err)
	}
}

func TestMutationDetectsNativeTagChangeAfterCLI(t *testing.T) {
	b := fixture(t)
	ctx := context.Background()
	req := createRequest()
	created, err := b.Run(ctx, req, false, "")
	if err != nil || created.Instance == nil {
		t.Fatalf("fixture create failed: %+v %v", created, err)
	}
	if err := os.WriteFile(filepath.Join(b.StateDir, "native.json.mode"), []byte("change_operation_during_stop"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := b.Run(ctx, Request{Operation: "stop", Project: req.Project, Instance: req.Instance, UUID: created.Instance.UUID}, false, "")
	if err == nil || !strings.Contains(err.Error(), "operation_outcome_uncertain") {
		t.Fatalf("operation response claimed success after native identity tags changed: %+v %v", out, err)
	}
	// Native side effect occurred; do not pretend the command failed before mutation.
	observed, found, err := b.inspect(ctx, req.Project, req.Instance)
	if err != nil || !found || observed.Status != "Stopped" || observed.OperationID != "" {
		t.Fatalf("test did not produce mid-flight native tag change: %+v %v", observed, err)
	}
	journal, err := b.loadIntents()
	if err != nil || len(journal.Intents) != 1 || journal.Intents[0].State != "confirmed" || journal.Intents[0].NativeUUID != created.Instance.UUID {
		t.Fatalf("mid-flight identity mutation changed confirmed journal: %+v %v", journal, err)
	}
}
