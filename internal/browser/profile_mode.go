package browser

import (
	"errors"
)

// Isolated state is ephemeral in an Agent-owned disposable directory, not
// another Chromium installation or a filesystem/application sandbox.
func normalizeProfileMode(input string) (string, error) {
	switch input {
	case "", "shared":
		return "shared", nil
	case "isolated":
		return "isolated", nil
	default:
		return "", errors.New("profile_mode must be shared or isolated")
	}
}
