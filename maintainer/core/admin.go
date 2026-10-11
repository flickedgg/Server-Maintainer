package core

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Admin operations used by the Discord bot. Each one runs on the controller's
// Run goroutine, so it can never race with crash handling or an update.

// Server states reported by Status.
const (
	StateOnline       = "online"
	StateStarting     = "starting"
	StateUnresponsive = "unresponsive"
	StateRestarting   = "restarting" // down after a crash or failed start, restart scheduled
	StateStopped      = "stopped"    // stopped with /stop
)

type ServerStatus struct {
	State            string
	Uptime           time.Duration
	Map              string
	Players          int // humans
	Bots             int
	MaxPlayers       int
	PlayersKnown     bool
	Failures         int
	FailureThreshold int
	CrashStreak      int
	Installed        string
	Pending          string
	PendingSince     time.Time
	UpdatePolicy     string
	UpdatesEnabled   bool
	Checking         bool
	LastCheck        time.Time
}

type UpdateCheckResult struct {
	Installed string
	Latest    string
	Available bool
	Policy    string // when the update will be installed
}

type checkAnswer struct {
	res UpdateCheckResult
	err error
}

// do runs fn on the Run goroutine and waits for it to finish.
func (c *Controller) do(ctx context.Context, fn func(runCtx context.Context)) error {
	done := make(chan struct{})
	req := func(runCtx context.Context) {
		defer close(done)
		fn(runCtx)
	}
	select {
	case c.requests <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) Status(ctx context.Context) (ServerStatus, error) {
	var st ServerStatus
	err := c.do(ctx, func(context.Context) {
		now := c.Now()
		st = ServerStatus{
			FailureThreshold: c.cfg.Health.FailureThreshold,
			Failures:         c.failures,
			CrashStreak:      c.crashStreak,
			Installed:        c.installed,
			Pending:          c.pending,
			PendingSince:     c.pendingSince,
			UpdatesEnabled:   c.cfg.Update.Enabled,
			Checking:         c.checking,
			LastCheck:        c.lastCheck,
		}
		if c.pending != "" {
			st.UpdatePolicy = c.updatePolicy()
		}
		switch {
		case c.srv == nil && c.adminStopped:
			st.State = StateStopped
		case c.srv == nil:
			st.State = StateRestarting
		case c.failures > 0:
			st.State = StateUnresponsive
		case !c.healthy:
			st.State = StateStarting
		default:
			st.State = StateOnline
		}
		if c.srv != nil {
			st.Uptime = now.Sub(c.srv.Started())
		}
		if c.srv != nil && c.playersKnown(now) {
			st.PlayersKnown = true
			st.Map = c.lastInfo.Map
			st.Players = c.lastInfo.Humans()
			st.Bots = c.lastInfo.Bots
			st.MaxPlayers = c.lastInfo.MaxPlayers
		}
	})
	return st, err
}

// Restart restarts the server, or starts it again after Stop. A pending update
// is installed as part of the restart. Players get the usual chat countdown.
func (c *Controller) Restart(ctx context.Context, by string) error {
	var result error
	err := c.do(ctx, func(runCtx context.Context) {
		reason := "restart requested by " + by
		c.log.Info("restart requested", "by", by)
		c.setAdminStopped(false)
		if c.pending != "" && c.cfg.Update.Enabled && !c.checking {
			c.applyUpdate(runCtx, reason)
		} else {
			c.alert(LevelInfo, "CS2 server restarting", fmt.Sprintf("Requested by %s on Discord.", by))
			c.warnPlayers(runCtx, "Server restarting")
			if runCtx.Err() != nil {
				return
			}
			c.stopServer(reason)
			c.startServer(runCtx)
		}
		if c.srv == nil && c.lastStartErr != nil {
			result = fmt.Errorf("the server didn't start: %w", c.lastStartErr)
		}
	})
	return errors.Join(err, result)
}

// Stop stops the server and keeps it down, across maintainer restarts too,
// until Restart. Crash handling, updates and the daily restart leave it alone.
func (c *Controller) Stop(ctx context.Context, by string) error {
	return c.do(ctx, func(runCtx context.Context) {
		c.log.Warn("stop requested; the server stays down until /restart", "by", by)
		c.setAdminStopped(true)
		c.restartC = nil // cancel a pending crash restart
		c.alert(LevelWarn, "CS2 server stopped",
			fmt.Sprintf("Stopped by %s on Discord. It stays down until someone runs /restart.", by))
		if c.srv != nil {
			c.warnPlayers(runCtx, "Server shutting down")
			c.stopServer("stop requested by " + by)
		}
	})
}

// CheckUpdate asks SteamCMD for the latest build now. If a check is already
// running, it waits for that one. An available update follows the normal
// install policy (it may be installed right away if the server is empty).
func (c *Controller) CheckUpdate(ctx context.Context) (UpdateCheckResult, error) {
	answer := make(chan checkAnswer, 1)
	err := c.do(ctx, func(runCtx context.Context) {
		if !c.cfg.Update.Enabled {
			answer <- checkAnswer{err: errors.New("updates are disabled in the config (update.enabled)")}
			return
		}
		c.checkWaiters = append(c.checkWaiters, answer)
		c.startCheck(runCtx)
	})
	if err != nil {
		return UpdateCheckResult{}, err
	}
	select {
	case a := <-answer:
		return a.res, a.err
	case <-ctx.Done():
		return UpdateCheckResult{}, ctx.Err()
	}
}

func (c *Controller) setAdminStopped(v bool) {
	if c.adminStopped == v {
		return
	}
	c.adminStopped = v
	c.saveState(func(s *State) { s.StoppedByAdmin = v })
}
