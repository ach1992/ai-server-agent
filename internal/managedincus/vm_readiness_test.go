package managedincus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVMReadinessIsAdvisoryAndSpecificToVM(t *testing.T) {
	dir := t.TempDir()
	code := filepath.Join(dir, "OVMF_CODE_4M.fd")
	vars := filepath.Join(dir, "OVMF_VARS_4M.fd")
	patterns := []string{filepath.Join(dir, "OVMF_CODE*.fd")}
	varPatterns := []string{filepath.Join(dir, "OVMF_VARS*.fd")}

	// /dev/null is a known character device; never alter a real KVM device.
	missing := detectVMReadiness("/dev/null", patterns, varPatterns)
	if missing.Status != "prerequisite_not_detected" || missing.ReasonCode != "default_uefi_firmware_not_detected" ||
		!strings.Contains(missing.OperatorHint, "ovmf") || !strings.Contains(missing.Verification, "managed_environment") {
		t.Fatalf("missing firmware did not yield actionable advisory: %+v", missing)
	}
	if err := os.WriteFile(code, []byte("firmware"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vars, []byte("vars"), 0600); err != nil {
		t.Fatal(err)
	}
	present := detectVMReadiness("/dev/null", patterns, varPatterns)
	if present.Status != "prerequisites_observed_not_verified" || present.ReasonCode != "" {
		t.Fatalf("files were mistaken for unavailable or for VM launch proof: %+v", present)
	}
	noKVM := detectVMReadiness(filepath.Join(dir, "missing-kvm"), patterns, varPatterns)
	if noKVM.ReasonCode != "kvm_not_detected" || !strings.Contains(noKVM.OperatorHint, "Containers may still work") {
		t.Fatalf("missing KVM was misidentified: %+v", noKVM)
	}
	if !IsUEFIFirmwareFailure("Unable to run feature checks during QEMU initialization: Unable to locate a UEFI firmware") ||
		!IsUEFIFirmwareFailure("Couldn't find one of the required UEFI firmware files: []") ||
		IsUEFIFirmwareFailure("KVM accelerator unavailable") {
		t.Fatal("native firmware failure recognition is too broad/narrow")
	}
}

func TestIncusStatusReportsIndependentVMPrerequisiteObservation(t *testing.T) {
	b := fixture(t)
	b.vmProbe = func() VMReadiness {
		return VMReadiness{
			Status: "prerequisite_not_detected", ReasonCode: "default_uefi_firmware_not_detected",
			OperatorHint: UEFIRepairHint, Verification: vmRecheck,
		}
	}
	state := b.Ready(context.Background())
	if !state.Available || state.Status != "ready" ||
		state.VMReadiness == nil || state.VMReadiness.ReasonCode != "default_uefi_firmware_not_detected" {
		t.Fatalf("VM prerequisite was incorrectly elevated to total Incus failure: %+v", state)
	}
	list, err := b.Run(context.Background(), Request{Operation: "list", Project: "default"}, false, "")
	if err != nil || !list.Available {
		t.Fatalf("container listing was blocked by a VM-only prerequisite: %+v %v", list, err)
	}
}
