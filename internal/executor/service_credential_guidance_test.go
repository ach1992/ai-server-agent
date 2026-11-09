package executor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/serviceaccess"
)

// Exercises the real service_use credential preflight and its recovery path.
// The missing private secret is not a missing native SSH/SQL/Host package.
func TestServiceCredentialUnenrolledDoesNotTriggerHostPrerequisite(t *testing.T) {
	const secret = "never-expose-this-token"
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("authorized credential was not consumed after enrollment: %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ready"))
	}))
	defer service.Close()

	s := newSyncOutputTestServer(t)
	s.cfg.InstanceID = "asa_credential_test"
	st := serviceaccess.New(s.cfg.StateDir)
	meta, err := st.Register(serviceaccess.Registration{
		HostID: s.cfg.InstanceID, Service: "example", Endpoint: service.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	unready := useService(s, meta.ID, ServiceUseSpec{Path: "/"})
	if unready.OK || unready.ErrorCode != "service_credential_not_enrolled" ||
		unready.ReasonCode != "service_credential_not_enrolled" ||
		unready.ErrorClass != "state" || unready.PrerequisiteCode != "" ||
		!strings.Contains(unready.OperatorHint, "service_access") ||
		strings.Contains(unready.OperatorHint, secret) {
		t.Fatalf("unenrolled credential misclassified as optional host dependency: %+v", unready)
	}

	if _, err := st.Enroll(meta.ID, "token", "", secret, 30, false); err != nil {
		t.Fatal(err)
	}
	ready := useService(s, meta.ID, ServiceUseSpec{Path: "/"})
	if !ready.OK || ready.Output != "ready" {
		t.Fatalf("credential enrollment did not restore authorized service use: %+v", ready)
	}
}

func TestManagedCredentialResolveFailureSharedConsumers(t *testing.T) {
	for _, message := range []string{"service_credential_unavailable", "service_not_enrolled"} {
		out := managedErr(errors.New(message))
		if out.OK || out.ErrorClass != "state" ||
			out.ErrorCode != "service_credential_not_enrolled" ||
			out.PrerequisiteCode != "" || out.OperatorHint == "" {
			t.Fatalf("known unenrolled secret should not trigger host repair: %+v", out)
		}
	}
}
