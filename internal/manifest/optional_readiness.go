package manifest

import (
	"sync"

	"github.com/ach1992/ai-server-agent/internal/systemexec"
)

// An optional adapter's installed flag is a live, complete provenance check,
// not a cached path-existence hint. Several independent adapter trees contain
// thousands of files; serial verification made the authenticated environment
// endpoint exceed ordinary HTTP client deadlines. Run these immutable-count
// checks with bounded overlap, preserving every check on every Build call.
const (
	optionalTypeScript = iota
	optionalPythonTy
	optionalBashLSP
	optionalWebLSP
	optionalSQLLens
	optionalRustLSP
	optionalPHPActor
	optionalPythonDAP
	optionalJavaScriptDAP
	optionalProbeCount
	optionalProbeParallelism = 2
)

// Probe results are stored by stable index; only the owning goroutine writes
// its slot and all readers wait for completion. Each probe is independently
// required and runs fresh on every call. There is deliberately no cache,
// persistent watcher or downgrade from content/ownership verification.
func runOptionalProbes(checks [optionalProbeCount]func() bool) [optionalProbeCount]bool {
	var installed [optionalProbeCount]bool
	slots := make(chan struct{}, optionalProbeParallelism)
	var wg sync.WaitGroup
	for index := range checks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			installed[i] = checks[i]()
		}(index)
	}
	wg.Wait()
	return installed
}

func liveOptionalReadiness() [optionalProbeCount]bool {
	return runOptionalProbes([optionalProbeCount]func() bool{
		func() bool { _, err := systemexec.TypeScript7(); return err == nil },
		func() bool { _, err := systemexec.TyPython(); return err == nil },
		func() bool { _, err := systemexec.BashLSP(); return err == nil },
		func() bool { _, err := systemexec.WebLSP(); return err == nil },
		func() bool { _, err := systemexec.SQLLens(); return err == nil },
		func() bool { _, _, _, err := systemexec.RustAnalyzerResources(); return err == nil },
		func() bool { _, _, err := systemexec.PHPActor(); return err == nil },
		func() bool { _, _, err := systemexec.PythonDebugpy(); return err == nil },
		func() bool { _, _, err := systemexec.JavaScriptDAP(); return err == nil },
	})
}
