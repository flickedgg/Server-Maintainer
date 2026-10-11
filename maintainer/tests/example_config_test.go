package tests

import (
	"encoding/json"
	"strings"
	"testing"

	"cs2-maintainer/core"
)

// The documented example must always load, and its values must match the real
// defaults, so the comments in it never drift from the code.
func TestExampleConfigLoadsAndMatchesDefaults(t *testing.T) {
	cfg, _, err := core.LoadConfig("../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json doesn't load: %v", err)
	}
	def := core.DefaultConfig()
	checks := map[string][2]any{
		"health":                {cfg.Health, def.Health},
		"restart.backoff":       {cfg.Restart.BackoffMin, def.Restart.BackoffMin},
		"restart.stable_after":  {cfg.Restart.StableAfter, def.Restart.StableAfter},
		"restart.min_uptime":    {cfg.Restart.MinUptime, def.Restart.MinUptime},
		"news":                  {cfg.News, def.News},
		"log.level":             {cfg.Log.Level, def.Log.Level},
		"log.max_size_mb":       {cfg.Log.MaxSizeMB, def.Log.MaxSizeMB},
		"log.max_files":         {cfg.Log.MaxFiles, def.Log.MaxFiles},
		"log.show_server":       {cfg.Log.ShowServerOutput, def.Log.ShowServerOutput},
		"server.port":           {cfg.Server.Port, def.Server.Port},
		"server.stop_timeout":   {cfg.Server.StopTimeout, def.Server.StopTimeout},
		"server.console_window": {cfg.Server.ConsoleWindow, def.Server.ConsoleWindow},
		"update.max_defer":      {cfg.Update.MaxDefer, def.Update.MaxDefer},
		"update.warn_before":    {cfg.Update.WarnBefore, def.Update.WarnBefore},
		"update.check":          {cfg.Update.CheckInterval, def.Update.CheckInterval},
		"update.validate":       {cfg.Update.Validate, def.Update.Validate},
	}
	for name, c := range checks {
		got, _ := json.Marshal(c[0])
		want, _ := json.Marshal(c[1])
		if string(got) != string(want) {
			t.Errorf("%s: example has %s, default is %s", name, got, want)
		}
	}
}

func TestStripJSONComments(t *testing.T) {
	in := `{
  // line comment
  "url": "https://discord.com/api/webhooks/1/abc", // keeps the // inside the string
  /* block
     comment */ "quote": "say \"hi\" // not a comment",
  "n": 1 /* inline */
}`
	out := string(core.StripJSONComments([]byte(in)))
	if strings.Count(out, "\n") != strings.Count(in, "\n") || len(out) != len(in) {
		t.Error("stripping changed offsets or line numbers")
	}
	var v struct {
		URL   string `json:"url"`
		Quote string `json:"quote"`
		N     int    `json:"n"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if v.URL != "https://discord.com/api/webhooks/1/abc" || v.Quote != `say "hi" // not a comment` || v.N != 1 {
		t.Errorf("got %+v", v)
	}
}
