package systemexec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
)

const (
	PHPActorVersion    = "2026.10.07.0"
	PHPActorDir        = "/usr/local/lib/ai-server-agent/lsp/php/phpactor/2026.10.07.0"
	PHPActorPHAR       = PHPActorDir + "/phpactor.phar"
	PHPActorPHARSHA256 = "b28bf6166e016d539fc2f2aec18ef918b12590e436a6f1ccbdf30b5bbdbdb16b"
)

// PHPActor returns only the exact root-owned pinned PHAR and a recognized
// system-owned versioned PHP CLI path. PHP is optional and never part of the
// Agent control-plane dependency set. There is no PHP executable supplied
// by a repository, Composer vendor folder or client argument.
func PHPActor() (string, string, error) {
	if runtime.GOOS != "linux" {
		return "", "", errors.New("php_lsp_unsupported_platform")
	}
	php := First("/usr/bin/php8.4", "/usr/bin/php8.3", "/usr/bin/php8.2")
	if php == "" {
		return "", "", errors.New("php_lsp_trusted_php_not_found")
	}
	if !TrustedStaticFile(PHPActorPHAR) {
		return "", "", errors.New("php_lsp_untrusted_phar_resource")
	}
	if err := verifyPHPActorPHAR(PHPActorPHAR); err != nil {
		return "", "", err
	}
	return php, PHPActorPHAR, nil
}

func verifyPHPActorPHAR(path string) error {
	stat, err := os.Lstat(path)
	if err != nil || !stat.Mode().IsRegular() ||
		stat.Size() < 4<<20 || stat.Size() > 7<<20 {
		return errors.New("php_lsp_phar_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("php_lsp_phar_unreadable")
	}
	h := sha256.New()
	n, readErr := io.Copy(h, io.LimitReader(f, stat.Size()+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || n != stat.Size() {
		return errors.New("php_lsp_phar_changed_during_validation")
	}
	if hex.EncodeToString(h.Sum(nil)) != PHPActorPHARSHA256 {
		return errors.New("php_lsp_phar_checksum_mismatch")
	}
	return nil
}
