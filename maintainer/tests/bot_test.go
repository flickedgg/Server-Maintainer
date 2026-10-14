package tests

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"cs2-maintainer/core"
)

const managerRole = "123456789012345678"

// fakeControl records what the bot asked for.
type fakeControl struct {
	status   core.ServerStatus
	check    core.UpdateCheckResult
	err      error
	restarts []string
	stops    []string
}

func (f *fakeControl) Status(context.Context) (core.ServerStatus, error) { return f.status, f.err }
func (f *fakeControl) Restart(_ context.Context, by string) error {
	f.restarts = append(f.restarts, by)
	return f.err
}
func (f *fakeControl) Stop(_ context.Context, by string) error {
	f.stops = append(f.stops, by)
	return f.err
}
func (f *fakeControl) CheckUpdate(context.Context) (core.UpdateCheckResult, error) {
	return f.check, f.err
}

func TestBotOnlyAllowsTheManagerRole(t *testing.T) {
	bot := core.NewBot("token", managerRole, &fakeControl{}, discardLogger())
	if !bot.Authorized([]string{"1", managerRole}) {
		t.Error("member with the role was denied")
	}
	if bot.Authorized([]string{"1", "2"}) || bot.Authorized(nil) {
		t.Error("member without the role was allowed")
	}
	if core.NewBot("token", "", &fakeControl{}, discardLogger()).Authorized([]string{""}) {
		t.Error("an empty role ID must never authorize anyone")
	}
}

func TestBotRegistersExactlyFourGuildCommands(t *testing.T) {
	var names []string
	for _, c := range core.BotCommands {
		names = append(names, c.Name)
		if c.Contexts == nil || !slices.Equal(*c.Contexts, []discordgo.InteractionContextType{discordgo.InteractionContextGuild}) {
			t.Errorf("/%s should only be usable inside a server, where roles exist", c.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"checkupdate", "health", "restart", "stop"}) {
		t.Errorf("commands = %v", names)
	}
}

func TestBotExecute(t *testing.T) {
	ctx := context.Background()
	ctl := &fakeControl{status: core.ServerStatus{State: core.StateOnline, Map: "de_mirage", Players: 3, MaxPlayers: 12,
		PlayersKnown: true, Uptime: 2 * time.Hour, Installed: "100", UpdatesEnabled: true}}
	bot := core.NewBot("token", managerRole, ctl, discardLogger())

	if out := bot.Execute(ctx, "health", "@admin"); !strings.Contains(out, "Online") || !strings.Contains(out, "3/12 players") {
		t.Errorf("health: %q", out)
	}
	if out := bot.Execute(ctx, "restart", "@admin"); !strings.Contains(out, "restarted by @admin") || !slices.Equal(ctl.restarts, []string{"@admin"}) {
		t.Errorf("restart: %q, calls %v", out, ctl.restarts)
	}
	if out := bot.Execute(ctx, "stop", "@admin"); !strings.Contains(out, "until someone runs /restart") || len(ctl.stops) != 1 {
		t.Errorf("stop: %q", out)
	}

	ctl.check = core.UpdateCheckResult{Installed: "100", Latest: "100"}
	if out := bot.Execute(ctx, "checkupdate", "@admin"); !strings.Contains(out, "Up to date (build 100)") {
		t.Errorf("checkupdate, current: %q", out)
	}
	ctl.check = core.UpdateCheckResult{Installed: "100", Latest: "200", Available: true, Policy: "Installs when the server is empty."}
	if out := bot.Execute(ctx, "checkupdate", "@admin"); !strings.Contains(out, "100 → 200") || !strings.Contains(out, "when the server is empty") {
		t.Errorf("checkupdate, available: %q", out)
	}

	ctl.err = errors.New("steamcmd timed out")
	if out := bot.Execute(ctx, "checkupdate", "@admin"); !strings.Contains(out, "failed: steamcmd timed out") {
		t.Errorf("checkupdate error: %q", out)
	}
}

func TestFormatStatus(t *testing.T) {
	cases := map[string]struct {
		st   core.ServerStatus
		want []string
	}{
		"stopped":      {core.ServerStatus{State: core.StateStopped, Installed: "100"}, []string{"Stopped", "/restart", "Build 100", "updates are off"}},
		"crashing":     {core.ServerStatus{State: core.StateRestarting, CrashStreak: 3}, []string{"Down, restarting", "3 crashes in a row"}},
		"unresponsive": {core.ServerStatus{State: core.StateUnresponsive, Failures: 2, FailureThreshold: 4}, []string{"Not responding", "2/4"}},
		"starting":     {core.ServerStatus{State: core.StateStarting, Uptime: 40 * time.Second}, []string{"Starting", "40s"}},
		"pending update": {core.ServerStatus{State: core.StateOnline, Installed: "100", Pending: "200", UpdatesEnabled: true,
			PendingSince: time.Now().Add(-12 * time.Minute), UpdatePolicy: "Installs when the server is empty."}, []string{"update 200 waiting 12m", "when the server is empty"}},
		"bots": {core.ServerStatus{State: core.StateOnline, PlayersKnown: true, Map: "de_dust2", Players: 1, Bots: 4, MaxPlayers: 10}, []string{"1/10 players (+4 bots)"}},
	}
	for name, c := range cases {
		out := core.FormatStatus(c.st)
		for _, w := range c.want {
			if !strings.Contains(strings.ToLower(out), strings.ToLower(w)) {
				t.Errorf("%s: %q missing %q", name, out, w)
			}
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:               "45s",
		12*time.Minute + 5*time.Second: "12m",
		4*time.Hour + 12*time.Minute:   "4h12m",
		50*time.Hour + 30*time.Minute:  "2d2h",
	} {
		if got := core.HumanDuration(d); got != want {
			t.Errorf("HumanDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestBotConfigValidation(t *testing.T) {
	cases := map[string]string{
		"token without role": `{"install_dir": "x", "steamcmd_path": "s", "discord_bot_token": "abc"}`,
		"role not an ID":     `{"install_dir": "x", "steamcmd_path": "s", "discord_bot_token": "abc", "server_manager_role_id": "Admins"}`,
	}
	for name, body := range cases {
		if _, _, err := core.LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	ok := `{"install_dir": "x", "steamcmd_path": "s", "discord_bot_token": "abc", "server_manager_role_id": "` + managerRole + `"}`
	if _, _, err := core.LoadConfig(writeConfig(t, ok)); err != nil {
		t.Errorf("valid bot config rejected: %v", err)
	}
}

// --- Controller admin operations, through the same harness as the other controller tests.

func TestAdminStopKeepsServerDownUntilRestart(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	waitFor(t, "first launch", func() bool { return h.launches() == 1 })
	ctx := context.Background()

	if err := h.ctrl.Stop(ctx, "@admin"); err != nil {
		t.Fatal(err)
	}
	if h.server(0).stops.Load() != 1 {
		t.Fatal("server wasn't stopped through the normal stop path")
	}
	time.Sleep(150 * time.Millisecond) // longer than crash backoff and several health/eval ticks
	if n := h.launches(); n != 1 {
		t.Fatalf("server was started again %d time(s) after /stop", n-1)
	}
	st, _ := h.ctrl.Status(ctx)
	if st.State != core.StateStopped || !h.state.Get().StoppedByAdmin {
		t.Errorf("status %q, persisted %v", st.State, h.state.Get().StoppedByAdmin)
	}
	waitFor(t, "stop alert", func() bool { return h.webhook.has("CS2 server stopped") })

	if err := h.ctrl.Restart(ctx, "@admin"); err != nil {
		t.Fatal(err)
	}
	if h.launches() != 2 || h.state.Get().StoppedByAdmin {
		t.Errorf("restart after stop: launches=%d stopped=%v", h.launches(), h.state.Get().StoppedByAdmin)
	}
}

func TestAdminStopSurvivesMaintainerRestart(t *testing.T) {
	h := newHarness(t, nil)
	h.state.Update(func(s *core.State) { s.StoppedByAdmin = true })
	h.start()
	waitFor(t, "a few health ticks", func() bool {
		st, err := h.ctrl.Status(context.Background())
		return err == nil && st.State == core.StateStopped
	})
	time.Sleep(50 * time.Millisecond)
	if h.launches() != 0 {
		t.Error("server started although it was stopped with /stop before the maintainer restarted")
	}
}

func TestAdminRestartWithPlayersWarnsAndRestarts(t *testing.T) {
	h := newHarness(t, func(c *core.Config) { c.Update.WarnBefore = core.Duration{Duration: 20 * time.Millisecond} })
	h.startHealthyWithPlayers()
	if err := h.ctrl.Restart(context.Background(), "@admin"); err != nil {
		t.Fatal(err)
	}
	if h.launches() != 2 || h.server(0).stops.Load() != 1 {
		t.Errorf("launches=%d stops=%d", h.launches(), h.server(0).stops.Load())
	}
	if h.rconMsgs.Load() == 0 {
		t.Error("players weren't warned")
	}
	if h.updater.applyCount() != 0 {
		t.Error("restart installed an update although none was pending")
	}
}

func TestAdminRestartInstallsPendingUpdate(t *testing.T) {
	h := newHarness(t, func(c *core.Config) { c.Update.CheckInterval = core.Duration{Duration: 20 * time.Millisecond} })
	h.startHealthyWithPlayers() // players online, so the update waits
	h.updater.set("200")
	waitFor(t, "update pending", func() bool {
		st, _ := h.ctrl.Status(context.Background())
		return st.Pending == "200"
	})
	if err := h.ctrl.Restart(context.Background(), "@admin"); err != nil {
		t.Fatal(err)
	}
	if h.updater.applyCount() != 1 || h.launches() != 2 {
		t.Errorf("applies=%d launches=%d; /restart should install the pending update in the same restart",
			h.updater.applyCount(), h.launches())
	}
}

func TestAdminCheckUpdate(t *testing.T) {
	h := newHarness(t, nil)
	h.startHealthyWithPlayers()
	ctx := context.Background()

	r, err := h.ctrl.CheckUpdate(ctx)
	if err != nil || r.Available || r.Installed != "100" {
		t.Errorf("up to date: %+v, %v", r, err)
	}
	h.updater.set("200")
	r, err = h.ctrl.CheckUpdate(ctx)
	if err != nil || !r.Available || r.Latest != "200" || r.Policy == "" {
		t.Errorf("available: %+v, %v", r, err)
	}
}

func TestAdminCheckUpdateWhenUpdatesDisabled(t *testing.T) {
	h := newHarness(t, func(c *core.Config) { c.Update.Enabled = false })
	h.start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.ctrl.CheckUpdate(ctx); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("err = %v", err)
	}
}

func TestAdminStatusOnline(t *testing.T) {
	h := newHarness(t, nil)
	h.startHealthyWithPlayers()
	st, err := h.ctrl.Status(context.Background())
	if err != nil || st.State != core.StateOnline || !st.PlayersKnown || st.Players != 3 || st.Installed != "100" {
		t.Errorf("status %+v, %v", st, err)
	}
}

// Talks to the real Discord API, so it only runs with CS2M_LIVE_TESTS=1.
func TestBotRejectsBadTokenLive(t *testing.T) {
	if os.Getenv("CS2M_LIVE_TESTS") != "1" {
		t.Skip("set CS2M_LIVE_TESTS=1 to run against discord.com")
	}
	bot := core.NewBot("MTAwMDAwMDAwMDAwMDAwMDAw.invalid.token", managerRole, &fakeControl{}, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	err := bot.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "rejected the bot token") {
		t.Fatalf("err = %v; a bad token should fail fast instead of retrying", err)
	}
	t.Logf("rejected in %s: %v", time.Since(start).Round(time.Millisecond), err)
}
