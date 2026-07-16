package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// buildState holds the result of the one-time node binary build, shared by
// every cluster in a test run.
var buildState struct {
	once sync.Once
	path string
	err  error
}

// nodeBinary compiles cmd/node exactly once per test process, into a stable
// path under os.TempDir(), and returns it. Every cluster started in the same
// run reuses the same binary.
func nodeBinary() (string, error) {
	buildState.once.Do(func() {
		root, err := projectRoot()
		if err != nil {
			buildState.err = err
			return
		}

		dir := filepath.Join(os.TempDir(), "bluepods-harness")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			buildState.err = fmt.Errorf("create harness build dir %s:\n%w", dir, err)
			return
		}

		// The PID keeps concurrent test binaries' builds from sharing one output
		// path: without it, one process executing the binary while another is
		// mid-write (or has built a different revision) races on the same file.
		path := filepath.Join(dir, fmt.Sprintf("node-bin-%d", os.Getpid()))

		cmd := exec.Command("go", "build", "-o", path, "./cmd/node")
		cmd.Dir = root

		if out, err := cmd.CombinedOutput(); err != nil {
			buildState.err = fmt.Errorf("build cmd/node:\n%s\n%w", out, err)
			return
		}

		buildState.path = path
	})

	return buildState.path, buildState.err
}

// projectRoot walks up from the working directory to the module root, marked
// by go.mod.
func projectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd:\n%w", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod above %s", dir)
		}

		dir = parent
	}
}

// systemPodPath returns the path to the system pod WASM built at the module
// root, the same artifact every real-process test and scenario uses.
func systemPodPath() (string, error) {
	root, err := projectRoot()
	if err != nil {
		return "", err
	}

	path := filepath.Join(root, "pods", "pod-system", "build", "pod.wasm")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("system pod not built at %s (run: cd pods/pod-system && make release):\n%w", path, err)
	}

	return path, nil
}
