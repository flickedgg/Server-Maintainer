//go:build unix

package core

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// configureServerCmd puts the server in its own process group so a Ctrl+C in the
// maintainer's terminal doesn't reach it directly; the maintainer stops it instead.
func configureServerCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
}

// configureHelperCmd makes a cancelled helper (SteamCMD) die with all its children.
// steamcmd.sh starts the real binary as a child, so killing only the shell isn't enough.
func configureHelperCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func InitProcessJob() error                { return nil }
func bindServerLifetime(*os.Process) error { return nil }

// requestStop sends SIGINT, which makes the server save and exit.
func requestStop(p *ServerProcess) error {
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("sending SIGINT: %w", err)
	}
	return nil
}
