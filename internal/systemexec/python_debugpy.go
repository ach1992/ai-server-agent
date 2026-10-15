package systemexec

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
)

const (
	PythonDebugpyVersion = "1.8.22"
	PythonDebugpyDir     = "/usr/local/lib/ai-server-agent/dap/python/debugpy/1.8.22"
	PythonDebugpyModules = PythonDebugpyDir + "/package"
	// PyPI debugpy-1.8.22-py2.py3-none-any.whl SHA-256:
	// a9e9d3550e15ca479c59333e90845029190531f0cacfedab3b815a57bd913947
	// The installer must verify that wheel before root-owned extraction. This
	// complete extracted-tree checksum pins *all* transitive Python modules,
	// pydevd resources and optional native objects, not merely python3.
	pythonDebugpyModuleDigest = "6ed9b2518d31d65ba87c835e13b40b54d5ef11cc10376594c1d6c09fb84e49a0"
)

func verifyPythonDebugpyModules(path string, rootOwned bool) error {
	return verifyPinnedNodeModules(path, rootOwned, nodeModulePolicy{
		label: "python_debugpy", fileCount: 428, dirCount: 52, maxBytes: 36 << 20,
		digest: pythonDebugpyModuleDigest,
	})
}

// PythonDebugpy is passive readiness/provenance discovery. It never installs
// tools or imports a repository package and does not imply permitted attach.
// Production debugging is worker-only; Python >= 3.10 is optional on all
// platforms. A file's extension never authorizes running project setup hooks.
func PythonDebugpy() (interpreter, packageRoot string, err error) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", "", errors.New("python_dap_unsupported_platform")
	}
	interpreter = First("/usr/bin/python3.15", "/usr/bin/python3.14",
		"/usr/bin/python3.13", "/usr/bin/python3.12", "/usr/bin/python3.11", "/usr/bin/python3.10")
	if interpreter == "" {
		return "", "", errors.New("python_dap_trusted_python_3_10_plus_unavailable")
	}
	entry := filepath.Join(PythonDebugpyModules, "debugpy", "adapter", "__main__.py")
	if !TrustedStaticFile(entry) {
		return "", "", errors.New("python_dap_untrusted_adapter_package")
	}
	if err := verifyPythonDebugpyModules(PythonDebugpyModules, true); err != nil {
		return "", "", fmt.Errorf("python_dap_untrusted_dependency_tree: %w", err)
	}
	return interpreter, PythonDebugpyModules, nil
}
