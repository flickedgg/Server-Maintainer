package tests

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cs2-maintainer/core"
)

type fakeServer struct {
	started time.Time
	done    chan struct{}
	once    sync.Once
	stops   atomic.Int32
}

func newFakeServer() *fakeServer { return &fakeServer{started: time.Now(), done: make(chan struct{})} }

func (f *fakeServer) Done() <-chan struct{} { return f.done }
func (f *fakeServer) Err() error            { return errors.New("exit status 1") }
func (f *fakeServer) Started() time.Time    { return f.started }
func (f *fakeServer) exit()                 { f.once.Do(func() { close(f.done) }) }
func (f *fakeServer) Stop(time.Duration) error {
	f.stops.Add(1)
	f.exit()
	return nil
}

type fakeUpdater struct {
	mu        sync.Mutex
	installed string
	latest    string
	applies   int
}

func (u *fakeUpdater) Installed() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.installed, nil
}

func (u *fakeUpdater) Latest(context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.latest, nil
}

func (u *fakeUpdater) Apply(context.Context, bool) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.applies++
	u.installed = u.latest
	return u.installed, nil
}

func (u *fakeUpdater) set(latest string) {
	u.mu.Lock()
	u.latest = latest
	u.mu.Unlock()
}

func (u *fakeUpdater) applyCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.applies
}

type harness struct {
	t        *testing.T
	ctrl     *core.Controller
	updater  *fakeUpdater
	state    *core.StateStore
	webhook  *webhookRecorder
	mu       sync.Mutex
	servers  []*fakeServer
	humans   atomic.Int32
	failing  atomic.Bool
	probes   atomic.Int32
	rconMsgs atomic.Int32
}

// newHarness builds a controller with fast timings, a fake server, a fake
// updater and a fake Discord webhook.
func newHarness(t *testing.T, tweak func(*core.Config)) *harness {
	cfg := core.DefaultConfig()
	cfg.InstallDir = t.TempDir()
	cfg.Health.Interval = core.Duration{Duration: 10 * time.Millisecond}
	cfg.Health.Timeout = core.Duration{Duration: 5 * time.Millisecond}
	cfg.Health.FailureThreshold = 3
	cfg.Health.BootGrace = core.Duration{}
	cfg.Restart.BackoffMin = core.Duration{Duration: 10 * time.Millisecond}
	cfg.Restart.BackoffMax = core.Duration{Duration: 40 * time.Millisecond}
	cfg.Update.CheckInterval = core.Duration{Duration: time.Hour}
	cfg.Update.WarnBefore = core.Duration{}
	cfg.Update.MaxDefer = core.Duration{}
	if tweak != nil {
		tweak(cfg)
	}
	st, err := core.OpenState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, updater: &fakeUpdater{installed: "100", latest: "100"}, state: st, webhook: newWebhookRecorder(t)}
	notifier := core.NewDiscord(h.webhook.URL, discardLogger())
	t.Cleanup(func() { notifier.Close(time.Second) })
	c := core.NewController(cfg, discardLogger(), st, notifier, h.updater)
	c.EvalEvery = 10 * time.Millisecond
	c.RecheckEvery = 10 * time.Millisecond
	c.Launch = func() (core.ServerHandle, error) {
		s := newFakeServer()
		h.mu.Lock()
		h.servers = append(h.servers, s)
		h.mu.Unlock()
		return s, nil
	}
	c.Probe = func(context.Context) (core.A2SInfo, error) {
		h.probes.Add(1)
		if h.failing.Load() {
			return core.A2SInfo{}, errors.New("i/o timeout")
		}
		return core.A2SInfo{Players: int(h.humans.Load()), MaxPlayers: 10}, nil
	}
	c.RCON = func(context.Context, string) error { h.rconMsgs.Add(1); return nil }
	h.ctrl = c
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.ctrl.Run(ctx)
		close(done)
	}()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			h.t.Error("controller did not stop")
		}
	})
}

// startHealthyWithPlayers waits until the server has answered health checks
// with players online, so later updates arrive mid-game.
func (h *harness) startHealthyWithPlayers() {
	h.humans.Store(3)
	h.start()
	waitFor(h.t, "healthy", func() bool { return h.probes.Load() >= 3 })
}

func (h *harness) server(i int) *fakeServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i < len(h.servers) {
		return h.servers[i]
	}
	return nil
}

func (h *harness) launches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.servers)
}

func TestControllerRestartsAfterCrashAndAlertsAdmins(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	waitFor(t, "first launch", func() bool { return h.launches() == 1 })
	h.server(0).exit()
	waitFor(t, "restart after crash", func() bool { return h.launches() == 2 })
	if h.server(0).stops.Load() != 0 {
		t.Error("crashed server should not be stopped again")
	}
	waitFor(t, "crash recorded", func() bool { return h.state.Get().Crashes == 1 })
	waitFor(t, "crash alert", func() bool { return h.webhook.has("CS2 server crashed") })
	waitFor(t, "recovery alert", func() bool { return h.webhook.has("CS2 server is back up") })
}

func TestControllerBacksOffCrashLoop(t *testing.T) {
	h := newHarness(t, func(c *core.Config) {
		c.Restart.BackoffMin = core.Duration{Duration: 30 * time.Millisecond}
		c.Restart.BackoffMax = core.Duration{Duration: time.Second}
	})
	h.start()
	var gaps []time.Duration
	last := time.Now()
	for i := 0; i < 4; i++ {
		waitFor(t, "launch", func() bool { return h.launches() == i+1 })
		gaps = append(gaps, time.Since(last))
		last = time.Now()
		h.server(i).exit()
	}
	// Gaps after crashes 1, 2, 3 should roughly double: 30ms, 60ms, 120ms.
	if gaps[3] < 100*time.Millisecond || gaps[3] < gaps[1] {
		t.Errorf("backoff did not grow: %v", gaps)
	}
}

func TestControllerRestartsUnresponsiveServer(t *testing.T) {
	h := newHarness(t, nil)
	h.failing.Store(true)
	h.start()
	waitFor(t, "restart after health failures", func() bool { return h.launches() >= 2 })
	if h.server(0).stops.Load() != 1 {
		t.Error("hung server should be stopped through the normal stop path")
	}
	waitFor(t, "unresponsive alert", func() bool { return h.webhook.has("CS2 server unresponsive") })
}

func TestControllerBootGraceDelaysHealthRestarts(t *testing.T) {
	h := newHarness(t, func(c *core.Config) { c.Health.BootGrace = core.Duration{Duration: time.Hour} })
	h.failing.Store(true)
	h.start()
	waitFor(t, "probes", func() bool { return h.probes.Load() > 10 })
	if h.launches() != 1 {
		t.Error("server was restarted during its boot grace period")
	}
}

func TestControllerAppliesUpdateWhenEmpty(t *testing.T) {
	h := newHarness(t, nil)
	h.updater.set("200")
	h.start()
	waitFor(t, "update applied", func() bool { return h.updater.applyCount() == 1 })
	waitFor(t, "server relaunched", func() bool { return h.launches() == 2 })
	waitFor(t, "state saved", func() bool { return h.state.Get().BuildID == "200" })
	waitFor(t, "update alerts", func() bool {
		return h.webhook.has("CS2 server restarting for an update") && h.webhook.has("CS2 server updated")
	})
	time.Sleep(50 * time.Millisecond)
	if n := h.updater.applyCount(); n != 1 {
		t.Errorf("update applied %d times", n)
	}
}

func TestControllerDefersUpdateWhilePlayersOnline(t *testing.T) {
	h := newHarness(t, func(c *core.Config) { c.Update.CheckInterval = core.Duration{Duration: 20 * time.Millisecond} })
	h.startHealthyWithPlayers()
	h.updater.set("200")
	time.Sleep(200 * time.Millisecond)
	if h.updater.applyCount() != 0 {
		t.Fatal("update applied while players were online")
	}
	h.humans.Store(0)
	waitFor(t, "update once empty", func() bool { return h.updater.applyCount() == 1 })
}

func TestControllerForcesUpdateAfterMaxDefer(t *testing.T) {
	h := newHarness(t, func(c *core.Config) {
		c.Update.CheckInterval = core.Duration{Duration: 20 * time.Millisecond}
		c.Update.MaxDefer = core.Duration{Duration: 100 * time.Millisecond}
		c.Update.WarnBefore = core.Duration{Duration: 20 * time.Millisecond}
	})
	h.startHealthyWithPlayers()
	h.updater.set("200")
	waitFor(t, "forced update", func() bool { return h.updater.applyCount() == 1 })
	if h.rconMsgs.Load() == 0 {
		t.Error("players were not warned before the forced update")
	}
}

// A news post (or the server's console notice) means the update is official:
// install as soon as SteamCMD has the build, even with players online.
func TestControllerNewsHintUpdatesWithPlayersOnline(t *testing.T) {
	h := newHarness(t, nil) // regular checks only at startup, so only the hint can find the build
	h.startHealthyWithPlayers()
	h.updater.set("200")
	h.ctrl.Hint(`Steam news: "Counter-Strike 2 Update"`)
	waitFor(t, "update after the news hint", func() bool { return h.updater.applyCount() == 1 })
	waitFor(t, "announcement alert", func() bool { return h.webhook.has("CS2 update announced") })
}

// Steam's app info can lag the news, so the maintainer keeps checking after a
// hint, picking the build up when it appears.
func TestControllerNewsHintWaitsForSteamCMD(t *testing.T) {
	h := newHarness(t, nil)
	h.startHealthyWithPlayers()
	h.ctrl.Hint("Steam news")
	time.Sleep(50 * time.Millisecond)
	if h.updater.applyCount() != 0 {
		t.Fatal("applied with no new build")
	}
	h.updater.set("200")
	waitFor(t, "update once SteamCMD has the build", func() bool { return h.updater.applyCount() == 1 })
}

func TestControllerUrgencyExpires(t *testing.T) {
	h := newHarness(t, func(c *core.Config) { c.Update.CheckInterval = core.Duration{Duration: 20 * time.Millisecond} })
	h.ctrl.UrgentFor = 50 * time.Millisecond
	h.startHealthyWithPlayers()
	h.ctrl.Hint("Steam news") // but no new build shows up in time
	time.Sleep(150 * time.Millisecond)
	h.updater.set("200")
	time.Sleep(200 * time.Millisecond)
	if h.updater.applyCount() != 0 {
		t.Error("a stale announcement still forced an update with players online")
	}
}
