package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"
)

// ServerHandle is a running server. *ServerProcess implements it; tests use fakes.
type ServerHandle interface {
	Done() <-chan struct{}
	Err() error
	Started() time.Time
	Stop(timeout time.Duration) error
}

type UpdateBackend interface {
	Installed() (string, error)
	Latest(ctx context.Context) (string, error)
	Apply(ctx context.Context, validate bool) (string, error)
}

type healthResult struct {
	gen  int
	info A2SInfo
	err  error
}

type checkResult struct {
	latest string
	err    error
}

// Controller is the single owner of the server lifecycle. Crashes, health results,
// update checks and timers all arrive as events in Run, so two parts of the tool
// can never restart the server at the same time.
type Controller struct {
	cfg     *Config
	log     *slog.Logger
	state   *StateStore
	notify  *Discord
	updater UpdateBackend

	// Hooks and timings. NewController fills them in; tests replace them.
	Launch    func() (ServerHandle, error)
	Probe     func(ctx context.Context) (A2SInfo, error)
	RCON      func(ctx context.Context, command string) error // nil without an RCON password
	Now       func() time.Time
	EvalEvery time.Duration
	// RecheckEvery rate-limits extra update checks after an update notice,
	// since the server can print its notice repeatedly.
	RecheckEvery time.Duration
	// UrgentFor is how long an update notice (Steam news or the server console)
	// keeps the maintainer looking for the new build before giving up.
	UrgentFor time.Duration

	hints    chan string
	healthCh chan healthResult
	checkCh  chan checkResult

	// Everything below is only touched by the Run goroutine.
	srv         ServerHandle
	gen         int // bumps on every start; stale health results are dropped
	healthy     bool
	failures    int
	lastInfo    A2SInfo
	lastInfoAt  time.Time
	crashStreak int
	restartC    <-chan time.Time
	probing     bool
	recovering  bool // restarted after a crash or hang; alert when it is back up

	installed    string
	pending      string // build waiting to be installed
	pendingSince time.Time
	urgent       bool // Steam news or the server announced an update
	urgentUntil  time.Time
	updateFails  int
	validateNext bool
	retryAt      time.Time
	checking     bool
	lastCheck    time.Time
	lastDaily    string
}

func NewController(cfg *Config, log *slog.Logger, state *StateStore, notify *Discord, updater UpdateBackend) *Controller {
	c := &Controller{
		cfg:          cfg,
		log:          log,
		state:        state,
		notify:       notify,
		updater:      updater,
		Now:          time.Now,
		EvalEvery:    5 * time.Second,
		RecheckEvery: 30 * time.Second,
		UrgentFor:    30 * time.Minute,
		hints:        make(chan string, 1),
		healthCh:     make(chan healthResult, 1),
		checkCh:      make(chan checkResult, 1),
	}
	addr := cfg.QueryAddr()
	c.Probe = func(ctx context.Context) (A2SInfo, error) { return QueryA2SInfo(ctx, addr) }
	if pw := cfg.Server.RCONPassword; pw != "" {
		c.RCON = func(ctx context.Context, command string) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err := RCONExec(ctx, addr, pw, command)
			if errors.Is(err, ErrRCONNoResponse) {
				return nil
			}
			return err
		}
	}
	return c
}

// Hint tells the controller a CS2 update was announced, by Steam news or by the
// server's console. The update is installed as soon as SteamCMD sees the new
// build, after warning players. Safe to call from any goroutine; never blocks.
func (c *Controller) Hint(reason string) {
	select {
	case c.hints <- reason:
	default: // a hint is already queued
	}
}

// IsUpdateHint matches the console lines CS2 prints when Steam tells it a new build is out.
func IsUpdateHint(line string) bool {
	return strings.Contains(line, "MasterRequestRestart") ||
		strings.Contains(line, "needs to be restarted in order to receive the latest update")
}

// Run manages the server until ctx is cancelled, then stops it cleanly.
func (c *Controller) Run(ctx context.Context) {
	if at := c.cfg.Restart.DailyAt; at != nil {
		// Don't fire today's restart right away if we start after its time.
		if now := c.Now(); now.Hour()*60+now.Minute() >= at.minutes() {
			c.lastDaily = now.Format(time.DateOnly)
		}
	}
	c.prepareInstall(ctx)
	c.startServer(ctx)

	healthTick := time.NewTicker(c.cfg.Health.Interval.Duration)
	defer healthTick.Stop()
	evalTick := time.NewTicker(c.EvalEvery)
	defer evalTick.Stop()
	var updateC <-chan time.Time
	if c.cfg.Update.Enabled {
		t := time.NewTicker(c.cfg.Update.CheckInterval.Duration)
		defer t.Stop()
		updateC = t.C
		c.startCheck(ctx)
	}

	for {
		var exitC <-chan struct{}
		if c.srv != nil {
			exitC = c.srv.Done()
		}
		select {
		case <-ctx.Done():
			c.stopServer("maintainer is shutting down")
			return
		case <-exitC:
			c.onExit()
		case <-c.restartC:
			c.restartC = nil
			c.startServer(ctx)
		case <-healthTick.C:
			c.startProbe(ctx)
		case r := <-c.healthCh:
			c.probing = false
			c.onHealth(ctx, r)
		case <-updateC:
			c.startCheck(ctx)
		case reason := <-c.hints:
			c.onHint(ctx, reason)
		case r := <-c.checkCh:
			c.checking = false
			c.onCheck(ctx, r)
		case <-evalTick.C:
			c.evaluate(ctx)
		}
	}
}

func (c *Controller) prepareInstall(ctx context.Context) {
	id, err := c.updater.Installed()
	if err == nil {
		c.installed = id
		c.log.Info("installed CS2 build", "build", id)
		if c.state.Get().BuildID != id {
			c.saveState(func(s *State) { s.BuildID = id })
		}
		return
	}
	if !errors.Is(err, fs.ErrNotExist) || !c.cfg.Update.Enabled {
		c.log.Warn("could not read the installed build", "err", err)
		return
	}
	c.log.Info("CS2 is not installed yet; installing with SteamCMD (tens of GB, this takes a while)", "dir", c.cfg.InstallDir)
	got, err := c.updater.Apply(ctx, true)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Error("initial install failed; will retry", "err", err)
			c.alert(LevelError, "CS2 install failed", err.Error())
		}
		return
	}
	c.installed = got
	c.saveState(func(s *State) { s.BuildID = got; s.LastUpdateAt = c.Now() })
	c.log.Info("CS2 installed", "build", got)
}

func (c *Controller) startServer(ctx context.Context) {
	if ctx.Err() != nil || c.srv != nil {
		return
	}
	c.restartC = nil
	srv, err := c.Launch()
	if err != nil {
		c.crashStreak++
		delay := Backoff(c.crashStreak, c.cfg.Restart.BackoffMin.Duration, c.cfg.Restart.BackoffMax.Duration)
		c.log.Error("could not start the server", "err", err, "retry_in", delay)
		if c.crashStreak == 1 {
			c.alert(LevelError, "CS2 server failed to start", err.Error())
		}
		c.restartC = time.After(delay)
		return
	}
	c.srv = srv
	c.gen++
	c.healthy = false
	c.failures = 0
	c.saveState(func(s *State) { s.ServerStarts++ })
}

func (c *Controller) stopServer(reason string) {
	if c.srv == nil {
		return
	}
	c.log.Info("stopping server", "reason", reason)
	if err := c.srv.Stop(c.cfg.Server.StopTimeout.Duration); err != nil {
		c.log.Error("stopping server", "err", err)
	}
	c.srv = nil
	c.healthy = false
	c.failures = 0
}

func (c *Controller) onExit() {
	srv := c.srv
	c.srv = nil
	c.healthy = false
	uptime := c.Now().Sub(srv.Started())
	if uptime >= c.cfg.Restart.StableAfter.Duration {
		c.crashStreak = 0
	}
	c.crashStreak++
	delay := Backoff(c.crashStreak, c.cfg.Restart.BackoffMin.Duration, c.cfg.Restart.BackoffMax.Duration)
	c.log.Error("server exited unexpectedly", "err", srv.Err(), "uptime", uptime.Round(time.Second),
		"crash_streak", c.crashStreak, "restart_in", delay)
	c.saveState(func(s *State) { s.Crashes++ })
	switch {
	case c.crashStreak == 1:
		c.alert(LevelError, "CS2 server crashed", fmt.Sprintf("Exited after %s (%v). Restarting in %s.", uptime.Round(time.Second), srv.Err(), delay))
	case c.crashStreak%5 == 0:
		c.alert(LevelError, "CS2 server is crash-looping", fmt.Sprintf("%d crashes in a row. Next restart in %s. Check logs/server.log.", c.crashStreak, delay))
	}
	c.recovering = true
	c.restartC = time.After(delay)
}

func (c *Controller) startProbe(ctx context.Context) {
	if c.srv == nil || c.probing {
		return
	}
	c.probing = true
	gen := c.gen
	go func() {
		pctx, cancel := context.WithTimeout(ctx, c.cfg.Health.Timeout.Duration)
		defer cancel()
		info, err := c.Probe(pctx)
		c.healthCh <- healthResult{gen: gen, info: info, err: err}
	}()
}

func (c *Controller) onHealth(ctx context.Context, r healthResult) {
	if r.gen != c.gen || c.srv == nil {
		return // result for a server that is gone
	}
	now := c.Now()
	if r.err == nil {
		if !c.healthy && c.recovering {
			c.recovering = false
			c.alert(LevelSuccess, "CS2 server is back up", fmt.Sprintf("Answering queries on %s after %s.",
				r.info.Map, now.Sub(c.srv.Started()).Round(time.Second)))
		}
		if !c.healthy {
			c.log.Info("server is up", "map", r.info.Map, "players", r.info.Humans(), "bots", r.info.Bots,
				"max", r.info.MaxPlayers, "boot_time", now.Sub(c.srv.Started()).Round(time.Second))
		} else if c.failures > 0 {
			c.log.Info("server is responding again", "after_failures", c.failures)
		}
		c.healthy = true
		c.failures = 0
		c.lastInfo = r.info
		c.lastInfoAt = now
		return
	}
	if !c.healthy && now.Sub(c.srv.Started()) < c.cfg.Health.BootGrace.Duration {
		c.log.Debug("waiting for the server to finish booting", "err", r.err)
		return
	}
	c.failures++
	threshold := c.cfg.Health.FailureThreshold
	c.log.Warn("health check failed", "err", r.err, "failures", c.failures, "threshold", threshold)
	if c.failures < threshold {
		return
	}
	c.log.Error("server is not responding; restarting it", "failures", c.failures)
	c.alert(LevelWarn, "CS2 server unresponsive", fmt.Sprintf("No reply to %d health checks in a row. Restarting.", c.failures))
	c.stopServer("not responding to health checks")
	c.recovering = true
	c.startServer(ctx)
}

func (c *Controller) startCheck(ctx context.Context) {
	if c.checking || !c.cfg.Update.Enabled {
		return
	}
	c.checking = true
	c.lastCheck = c.Now()
	go func() {
		latest, err := c.updater.Latest(ctx)
		c.checkCh <- checkResult{latest: latest, err: err}
	}()
}

func (c *Controller) onHint(ctx context.Context, reason string) {
	if !c.cfg.Update.Enabled {
		return
	}
	if !c.urgent {
		c.log.Warn("CS2 update announced; installing as soon as SteamCMD has it", "source", reason)
		c.alert(LevelInfo, "CS2 update announced", reason+"\nInstalling as soon as SteamCMD has the new build.")
	}
	c.urgent = true
	c.urgentUntil = c.Now().Add(c.UrgentFor)
	if c.Now().Sub(c.lastCheck) >= c.RecheckEvery {
		c.startCheck(ctx)
	}
}

func (c *Controller) onCheck(ctx context.Context, r checkResult) {
	if r.err != nil {
		if ctx.Err() == nil {
			c.log.Warn("update check failed", "err", r.err)
		}
		return
	}
	c.refreshInstalled()
	if !BuildNewer(r.latest, c.installed) {
		c.log.Debug("server is up to date", "installed", c.installed, "latest", r.latest)
		if c.pending != "" {
			c.log.Info("pending update is no longer needed", "installed", c.installed)
			c.pending = ""
		}
		return
	}
	if c.pending != r.latest {
		if c.pending == "" {
			c.pendingSince = c.Now()
		}
		c.pending = r.latest
		c.log.Info("update available", "installed", c.installed, "latest", r.latest, "policy", c.updatePolicy())
		c.alert(LevelInfo, "CS2 update available",
			fmt.Sprintf("Build %s → %s. %s", orUnknown(c.installed), r.latest, c.updatePolicy()))
	}
	c.evaluate(ctx)
}

func (c *Controller) refreshInstalled() {
	id, err := c.updater.Installed()
	if err != nil || id == c.installed {
		return
	}
	c.installed = id
	c.saveState(func(s *State) { s.BuildID = id })
}

// evaluate makes the time-based decisions: applying a pending update and the
// daily restart. It runs on a short ticker and after every update check.
func (c *Controller) evaluate(ctx context.Context) {
	now := c.Now()
	if c.urgent && c.pending == "" {
		if now.After(c.urgentUntil) {
			c.urgent = false
			c.log.Warn("update was announced but SteamCMD still has no new build; back to regular checks", "waited", c.UrgentFor)
		} else if now.Sub(c.lastCheck) >= c.RecheckEvery {
			// Steam's app info can lag behind the announcement; keep asking.
			c.startCheck(ctx)
		}
	}
	if c.pending != "" && !c.checking && !now.Before(c.retryAt) {
		if reason := c.updateReason(now); reason != "" {
			c.applyUpdate(ctx, reason)
			return
		}
	}
	c.maybeDailyRestart(ctx, now)
}

// updateReason returns why the pending update can be applied now, or "" to keep waiting.
func (c *Controller) updateReason(now time.Time) string {
	maxDefer := c.cfg.Update.MaxDefer.Duration
	switch {
	case c.srv == nil:
		return "server is not running"
	case !c.healthy:
		return "server is still booting"
	case c.playersKnown(now) && c.lastInfo.Humans() == 0:
		return "server is empty"
	case c.cfg.Update.Window.Contains(now):
		return "inside the restart window"
	case c.urgent:
		return "update was announced"
	case maxDefer > 0 && now.Sub(c.pendingSince) >= maxDefer:
		return fmt.Sprintf("update has waited %s", maxDefer)
	}
	return ""
}

func (c *Controller) updatePolicy() string {
	var parts []string
	parts = append(parts, "Installs when the server is empty")
	if w := c.cfg.Update.Window; w.Enabled() {
		parts = append(parts, fmt.Sprintf("or between %s and %s", w.Start, w.End))
	}
	if d := c.cfg.Update.MaxDefer.Duration; d > 0 {
		parts = append(parts, fmt.Sprintf("or after %s at the latest", d))
	}
	return strings.Join(parts, ", ") + "."
}

func (c *Controller) playersKnown(now time.Time) bool {
	return !c.lastInfoAt.IsZero() && now.Sub(c.lastInfoAt) <= 3*c.cfg.Health.Interval.Duration
}

func (c *Controller) applyUpdate(ctx context.Context, reason string) {
	target := c.pending
	c.log.Info("applying update", "from", orUnknown(c.installed), "to", target, "reason", reason)
	c.alert(LevelInfo, "CS2 server restarting for an update",
		fmt.Sprintf("Installing build %s (%s).", target, reason))
	c.warnPlayers(ctx, "a CS2 update")
	if ctx.Err() != nil {
		return
	}
	c.stopServer("applying update")

	validate := c.cfg.Update.Validate || c.validateNext
	start := c.Now()
	got, err := c.updater.Apply(ctx, validate)
	if ctx.Err() != nil {
		return // shutting down; Run returns next
	}
	c.validateNext = err != nil
	if err == nil && BuildNewer(target, got) {
		// Steam's content servers can lag behind the app info. Not a broken
		// install, so retry without validate.
		err = fmt.Errorf("Steam served build %s, still older than %s", got, target)
	}
	if err != nil {
		c.updateFails++
		delay := Backoff(c.updateFails, 2*time.Minute, time.Hour)
		c.retryAt = c.Now().Add(delay)
		c.log.Error("update failed; restarting the server on its current build", "err", err, "retry_in", delay)
		c.alert(LevelError, "CS2 update failed", fmt.Sprintf("%v\nRetrying in %s.", err, delay))
	} else {
		prev := c.installed
		c.installed = got
		c.pending = ""
		c.urgent = false
		c.updateFails = 0
		c.retryAt = time.Time{}
		took := c.Now().Sub(start).Round(time.Second)
		c.saveState(func(s *State) { s.BuildID = got; s.LastUpdateAt = c.Now() })
		c.log.Info("update installed", "build", got, "took", took)
		c.alert(LevelSuccess, "CS2 server updated", fmt.Sprintf("Build %s → %s in %s.", orUnknown(prev), got, took))
	}
	c.startServer(ctx)
}

func (c *Controller) maybeDailyRestart(ctx context.Context, now time.Time) {
	at := c.cfg.Restart.DailyAt
	if at == nil {
		return
	}
	today := now.Format(time.DateOnly)
	if c.lastDaily == today || now.Hour()*60+now.Minute() < at.minutes() {
		return
	}
	c.lastDaily = today
	if c.srv == nil || now.Sub(c.srv.Started()) < c.cfg.Restart.MinUptime.Duration {
		return
	}
	c.log.Info("scheduled daily restart")
	c.alert(LevelInfo, "CS2 server restarting", "Scheduled daily restart.")
	c.warnPlayers(ctx, "a scheduled restart")
	if ctx.Err() != nil {
		return
	}
	c.stopServer("scheduled daily restart")
	c.startServer(ctx)
}

// warnPlayers counts down in chat before a planned restart. It returns at once
// when nobody is playing or RCON isn't configured.
func (c *Controller) warnPlayers(ctx context.Context, what string) {
	total := c.cfg.Update.WarnBefore.Duration
	if c.RCON == nil || c.srv == nil || total <= 0 || !c.healthy ||
		!c.playersKnown(c.Now()) || c.lastInfo.Humans() == 0 {
		return
	}
	done := c.srv.Done()
	for remaining := total; remaining > 0; {
		msg := fmt.Sprintf("say Server restarting for %s in %s", what, countdownText(remaining))
		if err := c.RCON(ctx, msg); err != nil {
			c.log.Warn("could not warn players over RCON", "err", err)
			return
		}
		step := remaining
		switch {
		case remaining > 30*time.Second:
			step = remaining - 30*time.Second
		case remaining > 10*time.Second:
			step = remaining - 10*time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-time.After(step):
		}
		remaining -= step
	}
}

func countdownText(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if m := int(d / time.Minute); m != 1 {
			return fmt.Sprintf("%d minutes", m)
		}
		return "1 minute"
	}
	return fmt.Sprintf("%d seconds", int(d.Round(time.Second)/time.Second))
}

func (c *Controller) alert(level, title, text string) {
	c.notify.Notify(Event{Level: level, Title: title, Text: text})
}

func (c *Controller) saveState(fn func(*State)) {
	if err := c.state.Update(fn); err != nil {
		c.log.Error("saving state", "err", err)
	}
}

// Backoff returns lo doubled for each attempt after the first, capped at hi.
func Backoff(attempt int, lo, hi time.Duration) time.Duration {
	d := lo
	for i := 1; i < attempt && d < hi; i++ {
		d *= 2
	}
	return min(d, hi)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
