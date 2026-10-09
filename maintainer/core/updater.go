package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SteamUpdater talks to SteamCMD. Calls are serialized because two SteamCMD
// instances sharing one install fight over its lock files.
type SteamUpdater struct {
	cfg *Config
	log *slog.Logger
	mu  sync.Mutex
}

func NewSteamUpdater(cfg *Config, log *slog.Logger) *SteamUpdater {
	return &SteamUpdater{cfg: cfg, log: log}
}

func (u *SteamUpdater) manifestPath() string {
	return filepath.Join(u.cfg.InstallDir, "steamapps", fmt.Sprintf("appmanifest_%d.acf", u.cfg.AppID))
}

// Installed reads the build ID of the files on disk from the app manifest.
func (u *SteamUpdater) Installed() (string, error) {
	path := u.manifestPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	root, err := ParseVDF(string(data))
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	id, ok := root.Get("AppState", "buildid")
	if !ok || id == "" {
		return "", fmt.Errorf("no buildid in %s", path)
	}
	return id, nil
}

// Latest asks Steam for the current build ID of the configured branch without
// touching the installed files.
func (u *SteamUpdater) Latest(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearAppInfoCache()
	out, runErr := u.run(ctx, u.cfg.Update.CheckTimeout.Duration, false,
		"+login", "anonymous", "+app_info_update", "1", "+app_info_print", strconv.Itoa(u.cfg.AppID), "+quit")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	info, err := ExtractAppInfo(out, u.cfg.AppID)
	if err != nil {
		if runErr != nil {
			return "", fmt.Errorf("steamcmd: %v (%w)", runErr, err)
		}
		return "", fmt.Errorf("%w; last output: %s", err, lastLines(out, 3))
	}
	id, ok := info.Get("depots", "branches", u.cfg.Branch, "buildid")
	if !ok || id == "" {
		return "", fmt.Errorf("branch %q has no buildid in the app info", u.cfg.Branch)
	}
	return id, nil
}

// Apply runs app_update and returns the build ID now on disk.
func (u *SteamUpdater) Apply(ctx context.Context, validate bool) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := os.MkdirAll(u.cfg.InstallDir, 0o755); err != nil {
		return "", err
	}
	args := []string{"+force_install_dir", u.cfg.InstallDir, "+login", "anonymous", "+app_update", strconv.Itoa(u.cfg.AppID)}
	if u.cfg.Branch != "public" {
		args = append(args, "-beta", u.cfg.Branch)
	}
	if validate {
		args = append(args, "validate")
	}
	args = append(args, "+quit")

	u.log.Info("running steamcmd app_update", "validate", validate)
	out, runErr := u.run(ctx, u.cfg.Update.ApplyTimeout.Duration, true, args...)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// SteamCMD's exit code is unreliable (it returns 7 after a self-update, for
	// example), so judge success by its output and by the manifest.
	id, err := u.Installed()
	if err != nil {
		return "", fmt.Errorf("reading manifest after app_update: %w (steamcmd: %v; %s)", err, runErr, lastLines(out, 3))
	}
	if !strings.Contains(out, "Success! App") {
		return id, fmt.Errorf("steamcmd did not report success: %s", lastLines(out, 3))
	}
	return id, nil
}

func (u *SteamUpdater) run(ctx context.Context, timeout time.Duration, logLines bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, u.cfg.SteamCMDPath, args...)
	if dir := steamcmdDir(u.cfg.SteamCMDPath); dir != "" {
		cmd.Dir = dir
	}
	out := &cappedBuffer{max: 16 << 20}
	var w io.Writer = out
	if logLines {
		lw := NewLineWriter(func(line string) {
			if line = strings.TrimSpace(line); line != "" {
				u.log.Debug("steamcmd", "out", line)
			}
		})
		defer lw.Flush()
		w = io.MultiWriter(out, lw)
	}
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 10 * time.Second
	configureHelperCmd(cmd)

	start := time.Now()
	err := cmd.Run()
	u.log.Debug("steamcmd finished", "took", time.Since(start).Round(time.Second), "err", err)
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("steamcmd timed out after %s", timeout)
	}
	return out.String(), err
}

// clearAppInfoCache removes SteamCMD's cached app info. Without this, SteamCMD
// often prints a stale build ID and updates go unnoticed.
func (u *SteamUpdater) clearAppInfoCache() {
	var candidates []string
	if dir := steamcmdDir(u.cfg.SteamCMDPath); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "appcache", "appinfo.vdf"))
	}
	if home, err := os.UserHomeDir(); err == nil && runtime.GOOS != "windows" {
		candidates = append(candidates,
			filepath.Join(home, ".steam", "steam", "appcache", "appinfo.vdf"),
			filepath.Join(home, ".local", "share", "Steam", "appcache", "appinfo.vdf"),
			filepath.Join(home, "Steam", "appcache", "appinfo.vdf"))
	}
	for _, p := range candidates {
		if err := os.Remove(p); err == nil {
			u.log.Debug("cleared steamcmd app info cache", "path", p)
		}
	}
}

// EnsureSteamClientSDK copies steamclient.so to ~/.steam/sdk64, where the Linux
// server looks for it. A missing copy is the usual cause of "failed to load steamclient".
func (u *SteamUpdater) EnsureSteamClientSDK() {
	if runtime.GOOS != "linux" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dst := filepath.Join(home, ".steam", "sdk64", "steamclient.so")
	var candidates []string
	if dir := steamcmdDir(u.cfg.SteamCMDPath); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "linux64", "steamclient.so"))
	}
	candidates = append(candidates,
		filepath.Join(home, ".steam", "steamcmd", "linux64", "steamclient.so"),
		filepath.Join(home, ".local", "share", "Steam", "steamcmd", "linux64", "steamclient.so"),
		filepath.Join(home, "Steam", "linux64", "steamclient.so"))
	for _, src := range candidates {
		si, err := os.Stat(src)
		if err != nil {
			continue
		}
		if di, err := os.Stat(dst); err == nil && di.Size() == si.Size() && !di.ModTime().Before(si.ModTime()) {
			return
		}
		if err := copyFile(src, dst); err != nil {
			u.log.Warn("could not install steamclient.so for the server", "err", err)
			return
		}
		u.log.Info("installed steamclient.so for the server", "from", src, "to", dst)
		return
	}
}

// steamcmdDir is the directory holding SteamCMD, resolving a bare name via PATH.
func steamcmdDir(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	return filepath.Dir(resolved)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	return finishAtomic(tmp, dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, " | ")
}

// BuildNewer reports whether build a is newer than b. Steam build IDs are
// increasing integers; anything unparsable falls back to "different".
func BuildNewer(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	ai, aerr := strconv.ParseUint(a, 10, 64)
	bi, berr := strconv.ParseUint(b, 10, 64)
	if aerr != nil || berr != nil {
		return a != b
	}
	return ai > bi
}
