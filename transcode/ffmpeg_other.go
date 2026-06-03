//go:build !unix

package transcode

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on platforms without POSIX process groups (e.g.
// Windows), where the process is killed directly.
func setProcessGroup(cmd *exec.Cmd) {}

// terminate kills the process directly; there is no graceful group signal.
func terminate(p *os.Process) error { return killProcess(p) }

// kill force-kills the process directly.
func kill(p *os.Process) error { return killProcess(p) }

func killProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
