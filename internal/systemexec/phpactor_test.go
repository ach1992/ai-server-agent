package systemexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPHPActorTrustedRootAndPublishedArtifact(t *testing.T) {
	if php, phar, err := PHPActor(); err == nil && (phar != PHPActorPHAR ||
		(php != "/usr/bin/php8.2" && php != "/usr/bin/php8.3" && php != "/usr/bin/php8.4")) {
		t.Fatalf("PHP LSP picked uncontrolled optional dependency: %q %q", php, phar)
	}
	proof := os.Getenv("AGENT_PHP_LSP_PROOF_PHAR")
	if proof != "" {
		if err := verifyPHPActorPHAR(proof); err != nil {
			t.Fatalf("upstream Phpactor 2026.10.07.0 artifact proof failed: %v", err)
		}
		if TrustedStaticFile(proof) {
			t.Fatal("worker-owned PHAR fixture incorrectly has root trust")
		}
	}
	fake := filepath.Join(t.TempDir(), "phpactor.phar")
	if err := os.WriteFile(fake, []byte("<?php echo 'untrusted';"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPHPActorPHAR(fake); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("unverified PHAR accepted: %v", err)
	}
}
