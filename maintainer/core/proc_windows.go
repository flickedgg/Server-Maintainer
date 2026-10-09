package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procResumeThread             = kernel32.NewProc("ResumeThread")
)

const ownConsoleSupported = true

const (
	createNewConsole = 0x00000010
	createSuspended  = 0x00000004
)

// startInOwnConsole starts the server in a new console window of its own, like
// running it from a .bat file: its output shows there and admins can type
// server commands into it. os/exec can't do this, because it always redirects
// the child's stdin/stdout, and CS2's console then fails on every input poll
// ("CTextConsoleWin::GetLine: !GetNumberOfConsoleInputEvents").
//
// The process starts suspended and is put in the kill-on-close job before it
// runs, so it can never outlive the maintainer, even if we die right here.
func startInOwnConsole(bin string, args []string, dir, title string, log *slog.Logger) (*os.Process, error) {
	cmdline := syscall.EscapeArg(bin)
	for _, a := range args {
		cmdline += " " + syscall.EscapeArg(a)
	}
	appP, err := syscall.UTF16PtrFromString(bin)
	if err != nil {
		return nil, err
	}
	cmdP, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	dirP, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	titleP, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return nil, err
	}
	si := &syscall.StartupInfo{Title: titleP}
	si.Cb = uint32(unsafe.Sizeof(*si))
	var pi syscall.ProcessInformation
	if err := syscall.CreateProcess(appP, cmdP, nil, nil, false,
		createNewConsole|createSuspended, nil, dirP, si, &pi); err != nil {
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}
	defer syscall.CloseHandle(pi.Thread)
	defer syscall.CloseHandle(pi.Process)

	if serverJob != 0 {
		if r, _, err := procAssignProcessToJobObject.Call(uintptr(serverJob), uintptr(pi.Process)); r == 0 {
			log.Warn("could not tie server lifetime to the maintainer; it may outlive a maintainer crash", "err", err)
		}
	}
	if r, _, err := procResumeThread.Call(uintptr(pi.Thread)); r == 0xFFFFFFFF {
		syscall.TerminateProcess(pi.Process, 1)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	// Our handle is still open, so the PID can't have been reused yet.
	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		syscall.TerminateProcess(pi.Process, 1)
		return nil, err
	}
	return proc, nil
}

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x2000
	processSetQuota                        = 0x0100
	processTerminate                       = 0x0001
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// serverJob is a Job Object with kill-on-close. The handle is never closed, so
// Windows closes it when the maintainer exits for any reason and kills the server.
var serverJob syscall.Handle

func InitProcessJob() error {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformationClass,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	serverJob = syscall.Handle(h)
	return nil
}

func bindServerLifetime(p *os.Process) error {
	if serverJob == 0 {
		return nil
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h)
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(serverJob), uintptr(h)); r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// configureServerCmd gives the server its own process group so a Ctrl+C in the
// maintainer's console (or from NSSM on service stop) doesn't reach it directly.
func configureServerCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// configureHelperCmd makes a cancelled helper (SteamCMD) die with all its children.
func configureHelperCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}

// requestStop sends "quit" over RCON. Windows has no SIGINT for a process in
// another console group, so RCON is the only clean way to stop the server.
func requestStop(p *ServerProcess) error {
	if p.rconPassword == "" {
		return errors.New("no rcon_password configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := RCONExec(ctx, p.rconAddr, p.rconPassword, "quit"); err != nil && !errors.Is(err, ErrRCONNoResponse) {
		return err
	}
	return nil
}
