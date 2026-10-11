package core

import "syscall"

// setParentDeathSignal kills the server if the maintainer dies, so an orphaned
// server can't keep holding the port when the maintainer is restarted.
func setParentDeathSignal(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
