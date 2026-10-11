package tests

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cs2-maintainer/core"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaultsAndPaths(t *testing.T) {
	path := writeConfig(t, `{
		"install_dir": "cs2",
		"steamcmd_path": "steamcmd",
		"discord_webhook": "https://discord.com/api/webhooks/1/abc",
		"server": {"args": ["+map", "de_mirage"], "rcon_password": "pw", "gslt": "TOKEN"},
		"update": {"window": {"start": "23:00", "end": "02:00"}},
		"health": {"interval": "20s"}
	}`)
	cfg, _, err := core.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if cfg.InstallDir != filepath.Join(dir, "cs2") || cfg.StateFile != filepath.Join(dir, "state.json") {
		t.Errorf("paths not resolved against the config dir: %q %q", cfg.InstallDir, cfg.StateFile)
	}
	if cfg.SteamCMDPath != "steamcmd" {
		t.Errorf("bare steamcmd name should stay a PATH lookup, got %q", cfg.SteamCMDPath)
	}
	if cfg.Health.Interval.Duration != 20*time.Second || cfg.Health.FailureThreshold != 4 {
		t.Errorf("defaults not merged: %+v", cfg.Health)
	}
	if cfg.News.Title != "Counter-Strike 2 Update" || cfg.News.MaxAge.Duration != 6*time.Hour {
		t.Errorf("news defaults: %+v", cfg.News)
	}
	args := cfg.ServerArgs()
	for _, want := range []string{"-dedicated", "-usercon", "+map", "-port", "27015", "+rcon_password", "+sv_setsteamaccount"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
	red := strings.Join(core.RedactArgs(args), " ")
	if strings.Contains(red, "pw") || strings.Contains(red, "TOKEN") {
		t.Errorf("secrets not redacted: %s", red)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"unknown key":        `{"install_dir": "x", "steamcmd_path": "s", "helth": {}}`,
		"old discord key":    `{"install_dir": "x", "steamcmd_path": "s", "discord": {"webhook_url": "https://x"}}`,
		"bad duration":       `{"install_dir": "x", "steamcmd_path": "s", "health": {"interval": 15}}`,
		"port range":         `{"install_dir": "x", "steamcmd_path": "s", "server": {"port": 70000}}`,
		"timeout >= period":  `{"install_dir": "x", "steamcmd_path": "s", "health": {"interval": "2s", "timeout": "2s"}}`,
		"half window":        `{"install_dir": "x", "steamcmd_path": "s", "update": {"window": {"start": "04:00"}}}`,
		"bad clock":          `{"install_dir": "x", "steamcmd_path": "s", "update": {"window": {"start": "25:00", "end": "01:00"}}}`,
		"secret in args":     `{"install_dir": "x", "steamcmd_path": "s", "server": {"args": ["+rcon_password", "x"]}}`,
		"port in args":       `{"install_dir": "x", "steamcmd_path": "s", "server": {"args": ["-port", "1"]}}`,
		"http webhook":       `{"install_dir": "x", "steamcmd_path": "s", "discord_webhook": "http://x"}`,
		"missing install":    `{"steamcmd_path": "s"}`,
		"hammering steamcmd": `{"install_dir": "x", "steamcmd_path": "s", "update": {"check_interval": "5s"}}`,
		"empty news title":   `{"install_dir": "x", "steamcmd_path": "s", "news": {"title": " "}}`,
		"news too often":     `{"install_dir": "x", "steamcmd_path": "s", "news": {"interval": "1s"}}`,
		"absolute gameinfo":  `{"install_dir": "x", "steamcmd_path": "s", "gameinfo_search_paths": ["/csgo/addons/metamod"]}`,
		"gameinfo spaces":    `{"install_dir": "x", "steamcmd_path": "s", "gameinfo_search_paths": ["csgo/addons/meta mod"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := core.LoadConfig(writeConfig(t, body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNewsSettingsIgnoredWhenUpdatesOff(t *testing.T) {
	_, _, err := core.LoadConfig(writeConfig(t, `{"install_dir": "x", "update": {"enabled": false}, "news": {"interval": "1s"}}`))
	if err != nil {
		t.Errorf("news only triggers updates, so its settings shouldn't matter with updates off: %v", err)
	}
}

func TestEnvOverridesSecrets(t *testing.T) {
	t.Setenv("CS2M_RCON_PASSWORD", "from-env")
	t.Setenv("CS2M_DISCORD_WEBHOOK", "https://discord.com/api/webhooks/2/env")
	cfg, _, err := core.LoadConfig(writeConfig(t, `{"install_dir": "x", "steamcmd_path": "s"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.RCONPassword != "from-env" || cfg.DiscordWebhook != "https://discord.com/api/webhooks/2/env" {
		t.Errorf("env not applied: rcon=%q webhook=%q", cfg.Server.RCONPassword, cfg.DiscordWebhook)
	}
}

func TestWindowContains(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	day := core.Window{Start: &core.ClockTime{Hour: 4}, End: &core.ClockTime{Hour: 6}}
	night := core.Window{Start: &core.ClockTime{Hour: 23}, End: &core.ClockTime{Hour: 2}}
	cases := []struct {
		w    core.Window
		t    time.Time
		want bool
	}{
		{day, at(4, 0), true},
		{day, at(5, 59), true},
		{day, at(6, 0), false},
		{day, at(3, 59), false},
		{night, at(23, 30), true},
		{night, at(1, 0), true},
		{night, at(2, 0), false},
		{night, at(12, 0), false},
		{core.Window{}, at(5, 0), false},
	}
	for _, c := range cases {
		if got := c.w.Contains(c.t); got != c.want {
			t.Errorf("%v-%v contains %s = %v", c.w.Start, c.w.End, c.t.Format("15:04"), got)
		}
	}
}

func TestStateRoundTripAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := core.OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *core.State) { s.BuildID = "123"; s.NewsGID = "g1" }); err != nil {
		t.Fatal(err)
	}
	st2, err := core.OpenState(path)
	if err != nil || st2.Get().BuildID != "123" || st2.Get().NewsGID != "g1" {
		t.Fatalf("reload: %+v, %v", st2.Get(), err)
	}

	os.WriteFile(path, []byte(`{"build_id": "12`), 0o644)
	st3, err := core.OpenState(path)
	if !errors.Is(err, core.ErrStateCorrupt) || st3 == nil || st3.Get().BuildID != "" {
		t.Fatalf("corrupt state: st=%v err=%v", st3, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Error("corrupt file was not moved aside")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp-*"))
	if len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestStateAcceptsOldFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte(`{"build_id": null, "restarts_counter": 0}`), 0o644)
	if _, err := core.OpenState(path); err != nil {
		t.Fatal(err)
	}
}
