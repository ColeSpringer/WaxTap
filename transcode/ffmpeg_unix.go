//go:build unix

package transcode

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so ffmpeg and any
// helper children it spawns can be signaled at once.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the process group to exit cleanly (SIGTERM).
func terminate(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

// kill force-kills the process group (SIGKILL).
func kill(p *os.Process) error { return signalGroup(p, syscall.SIGKILL) }

// signalGroup sends sig to the process group led by p. A negative PID addresses
// the whole group, so children die with the leader.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return syscall.Kill(-p.Pid, sig)
}
