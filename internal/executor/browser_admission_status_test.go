package executor

import (
	"context"
	"testing"
)

func TestBrowserAdmissionStatusRequiresExecutorAuthentication(t *testing.T) {
	s, _, _ := testStdioBroker(t)
	s.token = "fixture-token"
	action := Request{Action: "browser_admission_status", Token: "bad"}
	if got := s.dispatchContext(context.Background(), action); got.OK || got.Error != "unauthorized" {
		t.Fatalf("unauthenticated status read: %+v", got)
	}
	action.Token = "fixture-token"
	if got := s.dispatchContext(context.Background(), action); !got.OK || got.Status != "available" {
		t.Fatalf("initial lease: %+v", got)
	}
	if !s.browserAdmission.try("opening") {
		t.Fatal("failed to reserve synthetic session")
	}
	if got := s.dispatchContext(context.Background(), action); !got.OK || got.Status != "reserved" || got.SessionID != "" {
		t.Fatalf("reserved lease not faithfully reported or owner leaked: %+v", got)
	}
	if !s.browserAdmission.transfer("opening", "stdio_test") {
		t.Fatal("session lease transfer failed")
	}
	if got := s.dispatchContext(context.Background(), action); !got.OK || got.Status != "reserved" {
		t.Fatalf("session lease not reported: %+v", got)
	}
	s.browserAdmission.release("stdio_test")
	if got := s.dispatchContext(context.Background(), action); !got.OK || got.Status != "available" {
		t.Fatalf("released lease still busy: %+v", got)
	}
}
