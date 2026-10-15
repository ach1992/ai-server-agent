package manifest

import (
	"strings"
	"testing"

	"github.com/ach1992/ai-server-agent/internal/config"
)

func TestEnvironmentPurposeManifestIsDescriptive(t *testing.T) {
	cfg := config.Default()
	cfg.ServerLabel = "QA آزمایش"
	dev := Build(cfg)
	if dev.EnvironmentProfile != "development" || dev.ServerLabel != cfg.ServerLabel {
		t.Fatalf("development identity unavailable: %+v", dev)
	}
	cfg.EnvironmentProfile = "validation"
	validation := Build(cfg)
	if validation.EnvironmentProfile != "validation" || validation.ServerLabel != cfg.ServerLabel {
		t.Fatalf("validation identity unavailable: %+v", validation)
	}
	if !strings.Contains(validation.Purpose, "candidate installation") ||
		!strings.Contains(validation.Purpose, "Git/development/root tools available") {
		t.Fatalf("validation guidance restricts tools or is missing: %q", validation.Purpose)
	}
	if len(validation.Critical) != len(dev.Critical) || len(validation.Optional) != len(dev.Optional) ||
		len(validation.Rules) != len(dev.Rules) {
		t.Fatal("environment purpose changed installed capabilities or safety instructions")
	}
	for i := range validation.Critical {
		if validation.Critical[i].Name != dev.Critical[i].Name {
			t.Fatal("environment purpose changed critical component list")
		}
	}
}
