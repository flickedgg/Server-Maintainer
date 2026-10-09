package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration written in JSON as a string such as "15s" or "10m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`duration must be a string such as "15s" or "10m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// ClockTime is a local time of day written as "HH:MM".
type ClockTime struct{ Hour, Minute int }

func (c *ClockTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`time of day must be a string such as "04:00"`)
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return fmt.Errorf("time of day %q must be HH:MM", s)
	}
	c.Hour, c.Minute = t.Hour(), t.Minute()
	return nil
}

func (c ClockTime) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }
func (c ClockTime) String() string               { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }
func (c ClockTime) minutes() int                 { return c.Hour*60 + c.Minute }

// Window is a daily local-time range. An end before the start crosses midnight.
type Window struct {
	Start *ClockTime `json:"start"`
	End   *ClockTime `json:"end"`
}

func (w Window) Enabled() bool { return w.Start != nil && w.End != nil }

func (w Window) Contains(t time.Time) bool {
	if !w.Enabled() {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	s, e := w.Start.minutes(), w.End.minutes()
	if s <= e {
		return m >= s && m < e
	}
	return m >= s || m < e
}

type Config struct {
	InstallDir   string `json:"install_dir"`
	SteamCMDPath string `json:"steamcmd_path"`
	AppID        int    `json:"app_id"`
	Branch       string `json:"branch"`
	StateFile    string `json:"state_file"`

	// DiscordWebhook receives admin alerts: maintainer failures, updates and
	// server restarts. Empty disables alerts.
	DiscordWebhook string `json:"discord_webhook"`

	// GameInfoSearchPaths are "Game" search paths (such as csgo/addons/metamod)
	// re-added to game/csgo/gameinfo.gi before every server start, because CS2
	// updates replace that file.
	GameInfoSearchPaths []string `json:"gameinfo_search_paths"`

	Server  ServerConfig  `json:"server"`
	Health  HealthConfig  `json:"health"`
	Update  UpdateConfig  `json:"update"`
	Restart RestartConfig `json:"restart"`
	News    NewsConfig    `json:"news"`
	Log     LogConfig     `json:"log"`
}

type ServerConfig struct {
	Binary       string   `json:"binary"` // optional; derived from install_dir when empty
	Args         []string `json:"args"`
	Port         int      `json:"port"`
	QueryHost    string   `json:"query_host"`
	RCONPassword string   `json:"rcon_password"`
	GSLT         string   `json:"gslt"`
	StopTimeout  Duration `json:"stop_timeout"`
	// ConsoleWindow (Windows only) runs the server in its own console window,
	// like a .bat file, instead of capturing its output into server.log.
	ConsoleWindow bool `json:"console_window"`
}

type HealthConfig struct {
	Interval         Duration `json:"interval"`
	Timeout          Duration `json:"timeout"`
	FailureThreshold int      `json:"failure_threshold"`
	BootGrace        Duration `json:"boot_grace"`
}

type UpdateConfig struct {
	Enabled       bool     `json:"enabled"`
	CheckInterval Duration `json:"check_interval"`
	Window        Window   `json:"window"`
	MaxDefer      Duration `json:"max_defer"` // 0 = wait for an empty server or the window forever
	WarnBefore    Duration `json:"warn_before"`
	Validate      bool     `json:"validate"` // validate on every update, not only after a failure
	CheckTimeout  Duration `json:"check_timeout"`
	ApplyTimeout  Duration `json:"apply_timeout"`
}

type RestartConfig struct {
	BackoffMin  Duration   `json:"backoff_min"`
	BackoffMax  Duration   `json:"backoff_max"`
	StableAfter Duration   `json:"stable_after"`
	DailyAt     *ClockTime `json:"daily_at"`
	MinUptime   Duration   `json:"min_uptime"`
}

// NewsConfig watches Steam announcements for update posts. A post whose title
// contains Title and whose date is within MaxAge of now triggers an update.
type NewsConfig struct {
	Enabled  bool     `json:"enabled"`
	Interval Duration `json:"interval"`
	Title    string   `json:"title"`
	MaxAge   Duration `json:"max_age"`
}

type LogConfig struct {
	Dir       string `json:"dir"`
	Level     string `json:"level"`
	MaxSizeMB int    `json:"max_size_mb"`
	MaxFiles  int    `json:"max_files"`
	// ShowServerOutput also prints the CS2 console to the maintainer's console,
	// prefixed with [cs2]. It always goes to server.log either way.
	ShowServerOutput bool `json:"show_server_output"`
}

func DefaultConfig() *Config {
	return &Config{
		AppID:     730,
		Branch:    "public",
		StateFile: "state.json",
		Server: ServerConfig{
			Port:          27015,
			QueryHost:     "127.0.0.1",
			StopTimeout:   Duration{30 * time.Second},
			ConsoleWindow: true,
		},
		Health: HealthConfig{
			Interval:         Duration{15 * time.Second},
			Timeout:          Duration{2 * time.Second},
			FailureThreshold: 4,
			BootGrace:        Duration{3 * time.Minute},
		},
		Update: UpdateConfig{
			Enabled:       true,
			CheckInterval: Duration{10 * time.Minute},
			MaxDefer:      Duration{30 * time.Minute},
			WarnBefore:    Duration{60 * time.Second},
			CheckTimeout:  Duration{5 * time.Minute},
			ApplyTimeout:  Duration{2 * time.Hour},
		},
		Restart: RestartConfig{
			BackoffMin:  Duration{5 * time.Second},
			BackoffMax:  Duration{5 * time.Minute},
			StableAfter: Duration{10 * time.Minute},
			MinUptime:   Duration{time.Hour},
		},
		News: NewsConfig{
			Enabled:  true,
			Interval: Duration{2 * time.Minute},
			Title:    "Counter-Strike 2 Update",
			MaxAge:   Duration{6 * time.Hour},
		},
		Log: LogConfig{
			Dir:              "logs",
			Level:            "info",
			MaxSizeMB:        20,
			MaxFiles:         5,
			ShowServerOutput: true,
		},
	}
}

// LoadConfig reads, fills defaults, applies env overrides, resolves paths relative
// to the config file, and validates. Warnings are problems that don't stop startup.
func LoadConfig(path string) (*Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading config: %w", err)
	}
	cfg := DefaultConfig()
	dec := json.NewDecoder(bytes.NewReader(StripJSONComments(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.applyEnv()
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	cfg.resolvePaths(base)
	warnings, err := cfg.Validate()
	return cfg, warnings, err
}

// StripJSONComments blanks out // and /* */ comments outside of strings, so the
// config can be documented inline. Comment bytes become spaces (newlines are
// kept), so decoder error offsets still point at the right place.
func StripJSONComments(data []byte) []byte {
	out := bytes.Clone(data)
	inString, escaped := false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			for i += 2; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
		}
	}
	return out
}

func (c *Config) applyEnv() {
	if v := os.Getenv("CS2M_RCON_PASSWORD"); v != "" {
		c.Server.RCONPassword = v
	}
	if v := os.Getenv("CS2M_GSLT"); v != "" {
		c.Server.GSLT = v
	}
	if v := os.Getenv("CS2M_DISCORD_WEBHOOK"); v != "" {
		c.DiscordWebhook = v
	}
}

func (c *Config) resolvePaths(base string) {
	abs := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	abs(&c.InstallDir)
	abs(&c.Server.Binary)
	abs(&c.StateFile)
	abs(&c.Log.Dir)
	// A bare "steamcmd" is looked up on PATH; anything with a separator is a path.
	if strings.ContainsAny(c.SteamCMDPath, `/\`) {
		abs(&c.SteamCMDPath)
	}
}

func (c *Config) Validate() (warnings []string, err error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	atLeast := func(name string, d Duration, min time.Duration) {
		if d.Duration < min {
			bad("%s must be at least %s (got %s)", name, min, d.Duration)
		}
	}

	if c.InstallDir == "" {
		bad("install_dir is required")
	}
	if c.SteamCMDPath == "" && c.Update.Enabled {
		bad("steamcmd_path is required when update.enabled is true")
	}
	if c.AppID <= 0 {
		bad("app_id must be positive")
	}
	if c.Branch == "" {
		bad("branch must not be empty")
	}
	if c.StateFile == "" {
		bad("state_file must not be empty")
	}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		bad("server.port must be 1-65535 (got %d)", c.Server.Port)
	}
	if c.Server.QueryHost == "" {
		bad("server.query_host must not be empty")
	}
	atLeast("server.stop_timeout", c.Server.StopTimeout, time.Second)
	for _, p := range c.GameInfoSearchPaths {
		if p == "" || strings.ContainsAny(p, " \t\"") || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
			bad("gameinfo_search_paths entry %q must be a relative path such as \"csgo/addons/metamod\"", p)
		}
	}
	for _, a := range c.Server.Args {
		switch strings.ToLower(a) {
		case "-port":
			bad("remove -port from server.args and set server.port instead")
		case "+rcon_password":
			bad("remove +rcon_password from server.args and set server.rcon_password instead")
		case "+sv_setsteamaccount":
			bad("remove +sv_setsteamaccount from server.args and set server.gslt instead")
		}
	}

	atLeast("health.interval", c.Health.Interval, time.Second)
	atLeast("health.timeout", c.Health.Timeout, 100*time.Millisecond)
	if c.Health.Timeout.Duration >= c.Health.Interval.Duration {
		bad("health.timeout must be shorter than health.interval")
	}
	if c.Health.FailureThreshold < 1 {
		bad("health.failure_threshold must be at least 1")
	}
	atLeast("health.boot_grace", c.Health.BootGrace, 0)

	if c.Update.Enabled {
		atLeast("update.check_interval", c.Update.CheckInterval, time.Minute)
		atLeast("update.check_timeout", c.Update.CheckTimeout, 30*time.Second)
		atLeast("update.apply_timeout", c.Update.ApplyTimeout, 5*time.Minute)
	}
	atLeast("update.max_defer", c.Update.MaxDefer, 0)
	atLeast("update.warn_before", c.Update.WarnBefore, 0)
	if (c.Update.Window.Start == nil) != (c.Update.Window.End == nil) {
		bad("update.window needs both start and end")
	}

	atLeast("restart.backoff_min", c.Restart.BackoffMin, 100*time.Millisecond)
	if c.Restart.BackoffMax.Duration < c.Restart.BackoffMin.Duration {
		bad("restart.backoff_max must be >= restart.backoff_min")
	}
	atLeast("restart.stable_after", c.Restart.StableAfter, time.Second)
	atLeast("restart.min_uptime", c.Restart.MinUptime, 0)

	if c.News.Enabled && c.Update.Enabled { // news only triggers updates
		atLeast("news.interval", c.News.Interval, 30*time.Second)
		atLeast("news.max_age", c.News.MaxAge, time.Minute)
		if strings.TrimSpace(c.News.Title) == "" {
			bad("news.title must not be empty")
		}
	}

	if c.DiscordWebhook != "" {
		u, err := url.Parse(c.DiscordWebhook)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			bad("discord_webhook must be an https URL")
		}
	} else {
		warnings = append(warnings, "no discord_webhook: admins won't be alerted about crashes, restarts or updates")
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.Log.Level)); err != nil {
		bad("log.level must be debug, info, warn or error")
	}
	if c.Log.MaxSizeMB < 1 {
		bad("log.max_size_mb must be at least 1")
	}
	if c.Log.MaxFiles < 1 {
		bad("log.max_files must be at least 1")
	}

	// Environment checks are warnings so the config can be validated on another machine.
	if runtime.GOOS == "windows" && c.Server.RCONPassword == "" {
		warnings = append(warnings, "no server.rcon_password: on Windows the server can only be force-killed, so it won't shut down cleanly")
	}
	if _, err := os.Stat(c.ServerBinary()); err != nil && !c.Update.Enabled {
		warnings = append(warnings, fmt.Sprintf("server binary not found at %s", c.ServerBinary()))
	}
	if c.Update.Enabled && c.SteamCMDPath != "" {
		if _, err := exec.LookPath(c.SteamCMDPath); err != nil {
			warnings = append(warnings, fmt.Sprintf("steamcmd not found at %s", c.SteamCMDPath))
		}
	}
	if c.Server.GSLT == "" {
		warnings = append(warnings, "no server.gslt: the server won't be listed in the public server browser")
	}
	return warnings, errors.Join(errs...)
}

func (c *Config) GameInfoPath() string {
	return filepath.Join(c.InstallDir, "game", "csgo", "gameinfo.gi")
}

func (c *Config) ServerBinary() string {
	if c.Server.Binary != "" {
		return c.Server.Binary
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(c.InstallDir, "game", "bin", "win64", "cs2.exe")
	}
	return filepath.Join(c.InstallDir, "game", "bin", "linuxsteamrt64", "cs2")
}

// ServerArgs returns the full argument list, including the managed port and secrets.
func (c *Config) ServerArgs() []string {
	args := slices.Clone(c.Server.Args)
	if !hasArg(args, "-dedicated") {
		args = append([]string{"-dedicated"}, args...)
	}
	args = append(args, "-port", strconv.Itoa(c.Server.Port))
	if c.Server.RCONPassword != "" {
		if !hasArg(args, "-usercon") {
			args = append(args, "-usercon")
		}
		args = append(args, "+rcon_password", c.Server.RCONPassword)
	}
	if c.Server.GSLT != "" {
		args = append(args, "+sv_setsteamaccount", c.Server.GSLT)
	}
	return args
}

func (c *Config) QueryAddr() string {
	return net.JoinHostPort(c.Server.QueryHost, strconv.Itoa(c.Server.Port))
}

var secretArgs = []string{"+rcon_password", "+sv_setsteamaccount"}

// RedactArgs hides the values that follow secret arguments, for logging.
func RedactArgs(args []string) []string {
	out := slices.Clone(args)
	for i := 0; i < len(out)-1; i++ {
		if slices.Contains(secretArgs, strings.ToLower(out[i])) {
			out[i+1] = "***"
		}
	}
	return out
}

func hasArg(args []string, name string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return strings.EqualFold(a, name) })
}
