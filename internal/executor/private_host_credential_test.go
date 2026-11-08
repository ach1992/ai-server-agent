package executor

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
	"github.com/ach1992/ai-server-agent/internal/policy"
	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

func TestPrivateHostCredentialFDAndTargetedDiagnostics(t *testing.T) {
	const private = "r00t-service-secret-123!"
	fd, err := provisionPrivateHostFD(private)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()

	script := "IFS= read -r password <&3\nprintf 'native stage=account-bootstrap reason=credential-accepted\n' >&2\nprintf 'secret-is-%s\n' \"$password\""
	command := newShellCommandContext(context.Background(), script, "/tmp", "/tmp")
	command.ExtraFiles = []*os.File{fd}
	command.Env = append(command.Env, "ASA_SERVICE_SECRET_FD=3")
	if strings.Contains(strings.Join(command.Args, " "), private) ||
		strings.Contains(strings.Join(command.Env, " "), private) {
		t.Fatal("secret leaked to process argv/environment")
	}
	var out bytes.Buffer
	filter := &credentialOutputRedactor{to: &out, secret: []byte(private)}
	command.Stdout, command.Stderr = filter, filter
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if err := filter.Flush(); err != nil {
		t.Fatal(err)
	}
	result := out.String()
	if strings.Contains(result, private) ||
		!strings.Contains(result, "stage=account-bootstrap") ||
		!strings.Contains(result, "reason=credential-accepted") ||
		!strings.Contains(result, "secret-is-[REDACTED]") {
		t.Fatalf("wrong private host credential delivery/diagnostics: %q", result)
	}
}

func TestPrivateRootCredentialRejectsUnboundPrincipalAndGuestBinding(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{InstanceID: "asa_host_admin_fixture", StateDir: dir}
	s := &Server{cfg: cfg, guard: policy.New(nil)}
	req := Request{Action: "run", Command: "true", Root: true, CredentialRef: "svc_nonsecret_ref"}
	denied := s.runContext(context.Background(), req)
	if denied.OK || denied.ErrorCode != "private_root_requires_authorized_principal" {
		t.Fatalf("unauthenticated protected root access accepted: %+v", denied)
	}
	st := serviceaccess.New(dir)
	metadata, err := st.Register(serviceaccess.Registration{
		HostID: cfg.InstanceID, Project: "qa", Guest: "guest-one",
		GuestUUID: "12345678-1234-1234-1234-123456789abc",
		Service:   "app", Username: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Generate(metadata.ID, "password", "owner", 7, false); err != nil {
		t.Fatal(err)
	}
	req.CredentialRef = metadata.ID
	req.PrincipalID, req.PrincipalClass = "direct-default", "direct"
	rejected := s.runContext(context.Background(), req)
	if rejected.OK || rejected.ErrorCode != "private_root_target_mismatch" {
		t.Fatalf("guest private credential reused on host: %+v", rejected)
	}
}
