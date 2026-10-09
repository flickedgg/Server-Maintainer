package core

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ServerProcess is one running instance of the CS2 dedicated server.
type ServerProcess struct {
	proc         *os.Process
	started      time.Time
	done         chan struct{}
	err          error // set before done is closed
	rconAddr     string
	rconPassword string
	log          *slog.Logger
}

// UsesConsoleWindow reports whether the server gets its own console window
// instead of having its output captured. Only Windows supports it.
func (c *Config) UsesConsoleWindow() bool {
	return ownConsoleSupported && c.Server.ConsoleWindow
}

// LaunchServer starts the server. With a console window (Windows), CS2 prints
// to that window and onLine is never called. Otherwise onLine gets every
// console line; it runs on the output-copying goroutine, so it must not block.
func LaunchServer(cfg *Config, log *slog.Logger, onLine func(string)) (*ServerProcess, error) {
	bin := cfg.ServerBinary()
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("server binary: %w", err)
	}
	args := cfg.ServerArgs()
	dir := filepath.Dir(bin)

	var proc *os.Process
	var wait func() error
	if cfg.UsesConsoleWindow() {
		title := fmt.Sprintf("CS2 server :%d (managed by cs2-maintainer)", cfg.Server.Port)
		p, err := startInOwnConsole(bin, args, dir, title, log)
		if err != nil {
			return nil, fmt.Errorf("starting server: %w", err)
		}
		proc = p
		wait = func() error {
			state, err := p.Wait()
			if err != nil {
				return err
			}
			if !state.Success() {
				return fmt.Errorf("exit status %d", state.ExitCode())
			}
			return nil
		}
	} else {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		out := NewLineWriter(onLine)
		cmd.Stdout, cmd.Stderr = out, out
		// If the server leaves a child holding the pipes, don't hang in Wait forever.
		cmd.WaitDelay = 5 * time.Second
		configureServerCmd(cmd)
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("starting server: %w", err)
		}
		if err := bindServerLifetime(cmd.Process); err != nil {
			log.Warn("could not tie server lifetime to the maintainer; it may outlive a maintainer crash", "err", err)
		}
		proc = cmd.Process
		wait = func() error {
			err := cmd.Wait()
			out.Flush()
			return err
		}
	}

	p := &ServerProcess{
		proc:         proc,
		started:      time.Now(),
		done:         make(chan struct{}),
		rconAddr:     cfg.QueryAddr(),
		rconPassword: cfg.Server.RCONPassword,
		log:          log,
	}
	log.Info("server started", "pid", proc.Pid, "console_window", cfg.UsesConsoleWindow(),
		"cmd", bin+" "+strings.Join(RedactArgs(args), " "))
	go func() {
		p.err = wait()
		close(p.done)
	}()
	return p, nil
}

func (p *ServerProcess) Done() <-chan struct{} { return p.done }
func (p *ServerProcess) Started() time.Time    { return p.started }

// Err is the exit error. Only valid after Done is closed.
func (p *ServerProcess) Err() error { return p.err }

// Stop asks the server to quit cleanly, waits up to timeout, then kills it.
func (p *ServerProcess) Stop(timeout time.Duration) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	start := time.Now()
	if err := requestStop(p); err != nil {
		p.log.Warn("could not ask the server to quit; killing it", "err", err)
	} else {
		select {
		case <-p.done:
			p.log.Info("server stopped cleanly", "took", time.Since(start).Round(time.Millisecond))
			return nil
		case <-time.After(timeout):
			p.log.Warn("server did not stop in time; killing it", "timeout", timeout)
		}
	}
	if err := p.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		p.log.Error("killing server", "err", err)
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("server process did not exit after kill")
	}
}
