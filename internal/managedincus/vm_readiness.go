package managedincus

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// VMReadiness is an advisory, passive host prerequisite observation, not
// proof that QEMU can launch a guest. The Incus daemon and containers can
// remain usable even when this VM-specific check reports a problem.
type VMReadiness struct {
	Status       string `json:"status"`
	ReasonCode   string `json:"reason_code,omitempty"`
	OperatorHint string `json:"operator_hint,omitempty"`
	Verification string `json:"verification"`
}

const vmRecheck = "After any authorized repair, recheck managed_environment(status); verify a real VM operation only when end-to-end proof is needed."

const UEFIRepairHint = "Inspect UEFI firmware discovery for Incus/QEMU. On Debian/Ubuntu amd64 check dpkg-query -W ovmf and /usr/share/OVMF; on arm64 check AAVMF/EDK2. Custom paths may work; repair only if needed and authorized."

// IsUEFIFirmwareFailure recognizes a native provider diagnostic without
// changing its original failure/uncertainty classification.
func IsUEFIFirmwareFailure(diagnostic string) bool {
	text := strings.ToLower(diagnostic)
	return strings.Contains(text, "unable to locate a uefi firmware") ||
		strings.Contains(text, "couldn't find one of the required uefi firmware files")
}

// This narrow helper is kept separate from the Incus backend's native
// operations so status never installs packages, changes services or reserves
// guest creation intent.
func hostVMReadiness() VMReadiness {
	var codePatterns, varsPatterns []string
	switch runtime.GOARCH {
	case "amd64":
		codePatterns = []string{"/usr/share/OVMF/OVMF_CODE*.fd", "/usr/share/qemu/OVMF_CODE*.fd", "/usr/share/ovmf/OVMF.fd"}
		varsPatterns = []string{"/usr/share/OVMF/OVMF_VARS*.fd", "/usr/share/qemu/OVMF_VARS*.fd"}
	case "arm64":
		codePatterns = []string{"/usr/share/AAVMF/AAVMF_CODE*.fd", "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd"}
		varsPatterns = []string{"/usr/share/AAVMF/AAVMF_VARS*.fd"}
	default:
		return VMReadiness{Status: "unverified", OperatorHint: "Verify native KVM and EDK2/UEFI support for this host architecture; no guest was launched.", Verification: vmRecheck}
	}
	return detectVMReadiness("/dev/kvm", codePatterns, varsPatterns)
}

func detectVMReadiness(kvm string, codePatterns, varsPatterns []string) VMReadiness {
	result := VMReadiness{Status: "prerequisites_observed_not_verified", Verification: vmRecheck}
	if st, err := os.Stat(kvm); err != nil || st.Mode()&os.ModeDevice == 0 {
		result.Status = "prerequisite_not_detected"
		result.ReasonCode = "kvm_not_detected"
		result.OperatorHint = "Default KVM device not detected. If a VM is needed, inspect /dev/kvm and host virtualization. Containers may still work."
		return result
	}
	if !firmwareFileDetected(codePatterns) || !firmwareFileDetected(varsPatterns) {
		result.Status = "prerequisite_not_detected"
		result.ReasonCode = "default_uefi_firmware_not_detected"
		result.OperatorHint = UEFIRepairHint + " Check result is advisory, not proof that VM creation is impossible."
	}
	return result
}

func firmwareFileDetected(patterns []string) bool {
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, path := range matches {
			if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
				return true
			}
		}
	}
	return false
}
