// Command cs2-maintainer keeps a Counter-Strike 2 dedicated server running,
// healthy and up to date.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"cs2-maintainer/core"
)

var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", "config.json", "path to the config file")
	validate := flag.Bool("validate", false, "check the config and exit")
	checkUpdate := flag.Bool("check-update", false, "print the installed and latest CS2 build and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("cs2-maintainer", version)
		return 0
	}

	cfg, warnings, err := core.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error:\n%v\n", err)
		return 2
	}
	if *validate {
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		fmt.Println("config OK")
		fmt.Println("server:", cfg.ServerBinary(), core.RedactArgs(cfg.ServerArgs()))
		return 0
	}

	log, serverLog, closeLogs, err := core.SetupLogging(cfg.Log)
	if err != nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
		alertFailure(core.NewDiscord(cfg.DiscordWebhook, log), log, "could not open log files", err)
		return 1
	}
	defer closeLogs()
	for _, w := range warnings {
		log.Warn("config: " + w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	updater := core.NewSteamUpdater(cfg, log)
	if *checkUpdate {
		return runCheckUpdate(ctx, cfg, updater)
	}

	notifier := core.NewDiscord(cfg.DiscordWebhook, log)
	defer notifier.Close(10 * time.Second)

	release, err := core.AcquireLock(cfg.StateFile + ".lock")
	if err != nil {
		// Another maintainer is running or the lock can't be created; that one is in charge.
		log.Error("cannot start", "err", err)
		return 1
	}
	defer release()

	state, err := core.OpenState(cfg.StateFile)
	if errors.Is(err, core.ErrStateCorrupt) {
		log.Warn("starting with fresh state", "err", err)
	} else if err != nil {
		alertFailure(notifier, log, "could not read the state file", err)
		return 1
	}

	if err := core.InitProcessJob(); err != nil {
		log.Warn("could not create a job object; the server may outlive a maintainer crash", "err", err)
	}

	ctrl := core.NewController(cfg, log, state, notifier, updater)
	var lastGameInfoErr string // Launch only runs on the controller goroutine
	ctrl.Launch = func() (core.ServerHandle, error) {
		updater.EnsureSteamClientSDK()
		ensureGameInfo(cfg, log, notifier, &lastGameInfoErr)
		p, err := core.LaunchServer(cfg, log, func(line string) {
			if core.IsConsoleNoise(line) {
				return
			}
			core.WriteConsoleLine(serverLog, line)
			if cfg.Log.ShowServerOutput {
				core.EchoServerLine(os.Stdout, line)
			}
			if core.IsUpdateHint(line) {
				ctrl.Hint("The server reported that a new CS2 build is out.")
			}
		})
		if err != nil {
			return nil, err // not a typed-nil *ServerProcess in the interface
		}
		return p, nil
	}

	log.Info("cs2-maintainer starting", "version", version, "os", runtime.GOOS,
		"install_dir", cfg.InstallDir, "port", cfg.Server.Port, "updates", cfg.Update.Enabled)
	if cfg.UsesConsoleWindow() {
		log.Info("the CS2 console runs in its own window, so server.log isn't written; set server.console_window to false to capture it")
	}
	notifier.Notify(core.Event{Level: core.LevelInfo, Title: "CS2 maintainer started",
		Text: fmt.Sprintf("Managing the server on port %d.", cfg.Server.Port)})

	var wg sync.WaitGroup
	if cfg.News.Enabled && cfg.Update.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			core.NewNewsWatcher(cfg, log, state, func(it core.NewsItem) {
				ctrl.Hint(fmt.Sprintf("Steam news: %q, posted %s.\n%s",
					it.Title, time.Unix(it.Date, 0).Format("2006-01-02 15:04 MST"), it.URL))
			}).Run(ctx)
		}()
	}

	if err := runController(ctx, ctrl); err != nil {
		stop()
		wg.Wait()
		alertFailure(notifier, log, "the maintainer crashed", err)
		return 1
	}
	stop()
	wg.Wait()
	log.Info("cs2-maintainer stopped")
	notifier.Notify(core.Event{Level: core.LevelWarn, Title: "CS2 maintainer stopped",
		Text: "The maintainer was shut down. The server is stopped and won't be restarted until it runs again."})
	return 0
}

// runController turns a panic in the controller into an error, so admins get an
// alert before the process exits (and systemd or NSSM restarts it).
func runController(ctx context.Context, ctrl *core.Controller) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	ctrl.Run(ctx)
	return nil
}

// ensureGameInfo re-adds the configured search paths (Metamod and friends) to
// gameinfo.gi before a start. On failure the server still starts, without
// plugins, and admins are alerted once per distinct error.
func ensureGameInfo(cfg *core.Config, log *slog.Logger, notifier *core.Discord, lastErr *string) {
	if len(cfg.GameInfoSearchPaths) == 0 {
		return
	}
	added, err := core.EnsureGameInfoSearchPaths(cfg.GameInfoPath(), cfg.GameInfoSearchPaths)
	if err != nil {
		log.Error("could not add search paths to gameinfo.gi; plugins won't load", "err", err)
		if msg := err.Error(); msg != *lastErr {
			*lastErr = msg
			notifier.Notify(core.Event{Level: core.LevelError, Title: "gameinfo.gi could not be patched",
				Text: fmt.Sprintf("Starting the server without %v, so plugins won't load.\n%v", cfg.GameInfoSearchPaths, err)})
		}
		return
	}
	*lastErr = ""
	if len(added) > 0 {
		log.Info("added search paths to gameinfo.gi (CS2 updates reset this file)", "paths", added)
		notifier.Notify(core.Event{Level: core.LevelInfo, Title: "gameinfo.gi patched",
			Text: fmt.Sprintf("Re-added %v. CS2 updates reset this file.", added)})
	}
}

// alertFailure logs a fatal error and waits briefly for Discord to accept the alert.
func alertFailure(notifier *core.Discord, log *slog.Logger, what string, err error) {
	log.Error("cs2-maintainer failed: "+what, "err", err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := fmt.Sprintf("%s: %v", what, err)
	if sendErr := notifier.Send(ctx, core.Event{Level: core.LevelError, Title: "CS2 maintainer failed", Text: msg, Time: time.Now()}); sendErr != nil {
		log.Error("could not alert admins on Discord", "err", sendErr)
	}
}

func runCheckUpdate(ctx context.Context, cfg *core.Config, u *core.SteamUpdater) int {
	installed, err := u.Installed()
	if err != nil {
		fmt.Println("installed: unknown:", err)
	} else {
		fmt.Println("installed:", installed)
	}
	latest, err := u.Latest(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checking latest build:", err)
		return 1
	}
	fmt.Printf("latest (%s): %s\n", cfg.Branch, latest)
	if core.BuildNewer(latest, installed) {
		fmt.Println("update available: yes")
	} else {
		fmt.Println("update available: no")
	}
	return 0
}
