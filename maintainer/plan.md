# CS2 Server Maintainer: Build Plan (v2)

Each phase produces something you can run and test before moving on.

## What changed from v1

These fix failure modes the first plan would have hit in production:

| Area | v1 | v2 | Why |
|---|---|---|---|
| Source of truth for build ID | State file | `steamapps/appmanifest_730.acf` | The state file can drift (manual updates, restored backups). The manifest is what is really installed. |
| Launch args | One command string | JSON array plus `port`, `gslt`, `rcon_password` fields | No shell quoting bugs. Secrets are redacted in logs and can come from env vars. |
| Stale SteamCMD app info | Not handled | Delete `appcache/appinfo.vdf` before each check | SteamCMD often returns a cached, old build ID, so updates are missed. |
| `validate` on every update | Always | Only after a failed update (or opt-in) | `validate` rehashes ~60 GB of files and turns a 2-minute update into 20+ minutes of downtime. |
| Waiting for an empty server | Unbounded | `max_defer` (default 30m) plus in-game RCON countdown | After a CS2 update, updated clients can't join an outdated server. Waiting forever leaves it dead. |
| Update detection | Polling only | Polling, plus Steam news ("Counter-Strike 2 Update" posts) and the console's `MasterRequestRestart` | News and the server learn about a new build first. React within minutes, not on the next poll. |
| A2S query | Plain `A2S_INFO` | `A2S_INFO` with challenge handling | Valve added a challenge step to `A2S_INFO` in 2020. Without it, every health check fails. |
| Player count | Raw count | `players - bots` | Bots kept "update only when empty" from ever firing. |
| Orphaned servers | Not handled | Linux: own process group plus `Pdeathsig`. Windows: Job Object with kill-on-close. | If the maintainer crashes, the old server can't keep holding the port. |
| Ctrl+C / service stop | Not handled | Server runs in its own process group | Ctrl+C in the console would otherwise hit the server directly and skip the clean save. |
| Two maintainers at once | Not handled | Lock file next to the state file | systemd plus a manual run would fight over the same server. |
| Crash backoff | "With backoff" | Exponential, capped, reset after `stable_after` uptime | A crash loop doesn't hammer the box, and a crash after days of uptime restarts fast. |
| News | Posted to Discord | Read only, as an update trigger | Players don't need the patch notes from us. A fresh update post is a reliable early update signal. |
| Discord | News plus events | Admin alerts only | The webhook tells admins when the maintainer fails, updates or restarts the server. |
| Tests | Next to the code | `tests/` folder, against the exported `core` API | Keeps the code folders clean. The logic moved from `package main` to `core`, since `main` can't be imported. |
| Scheduled restarts | Not covered (README promised them) | Optional `restart.daily_at` | |
| Mods after an update | Not covered | `gameinfo_search_paths` re-added to `gameinfo.gi` before every start | Updates and `validate` replace `gameinfo.gi`, which drops `Game csgo/addons/metamod` and with it every plugin. |
| Remote control | Not covered | Optional Discord bot: `/restart`, `/stop`, `/checkupdate`, `/health`, limited to one role | Admins can act from their phone. Commands go through the controller like any other event, so they can't race a crash restart or an update. |
| Linux `steamclient.so` | Not covered | Copied to `~/.steam/sdk64/` automatically | This is the most common reason a fresh Linux CS2 server fails to start. |
| First run | Assumes CS2 is installed | Installs CS2 if the manifest is missing | |
| Config paths | Relative to working dir | Relative to the config file | A service started from another directory still finds its files. |

## Phase 0: Setup and decisions

- Go 1.22 or newer.
- Module `cs2-maintainer`, no third-party dependencies. Layout: `main.go` handles flags and wiring, `core/` holds all logic, `tests/` holds the test suite.
- One install directory per server. SteamCMD and CS2 files live outside the tool's directory.
- Get a GSLT if the server should be listed in the public browser.
- Server binary: Linux `game/bin/linuxsteamrt64/cs2`, Windows `game/bin/win64/cs2.exe`. The working directory is the binary's folder.
- CS2 app ID: 730. RCON needs `-usercon`, which the tool adds automatically.

## Phase 1: Config and state

- **Config (JSON):** durations as strings (`"15s"`). Unknown keys are rejected so typos fail loudly. Every value is range-checked, and all errors are reported at once. Environment overrides: `CS2M_RCON_PASSWORD`, `CS2M_GSLT`, `CS2M_DISCORD_WEBHOOK`.
- **State file:** last build, news GID and date, counters. Atomic write: temp file, fsync, rename. A corrupt file is moved to `state.json.corrupt` and the tool starts fresh.
- **Logging:** `log/slog` to stdout and `logs/maintainer.log`. Server console output goes to `logs/server.log`. Both rotate by size.

**Test:** `cs2-maintainer -validate` and unit tests for bad values and state round trips.

## Phase 2: Supervisor

- Start with `os/exec` and stream output line by line into the server log. `WaitDelay` keeps a stuck pipe from hanging the tool.
- Exponential crash backoff (`backoff_min` up to `backoff_max`), reset after `stable_after` of uptime.
- Shutdown is driven by `context.Context` from SIGINT or SIGTERM.

## Phase 3: Stop logic (platform-specific)

- **Linux:** SIGINT to the server, wait `stop_timeout`, then SIGKILL.
- **Windows:** `quit` over Source RCON, wait, then terminate.
- `proc_unix.go`, `proc_linux.go` and `proc_windows.go` sit behind the same functions. Lifetime binding (`Pdeathsig` or Job Object) lives there too.

## Phase 4: Updater

- **Check:** delete the cached `appinfo.vdf`, run `+login anonymous +app_info_update 1 +app_info_print 730 +quit`, parse the VDF block, then read `depots.branches.<branch>.buildid`.
- **Installed build:** `appmanifest_730.acf`.
- **Apply:** `+force_install_dir <dir> +login anonymous +app_update 730 [-beta x] [validate] +quit`. Don't trust SteamCMD's exit code. Success requires "Success! App" in the output and a manifest build at or above the target.
- On failure, restart on the old build and retry with exponential backoff. The retry runs with `validate`.
- All SteamCMD calls are serialized, run in their own process group, and are killed on timeout.

## Phase 5: Health checks

- A2S_INFO with challenge every `interval`, using a short `timeout`.
- Boot grace: failures don't count until the first success or until `boot_grace` has passed.
- After `failure_threshold` consecutive failures, restart through the normal stop logic.

## Phase 6: Coordination

One controller goroutine owns the server. Every input is an event on a channel: exit, health result, check result, console hint, timers, shutdown. Slow work (A2S, SteamCMD checks) runs in goroutines that report back. Only the controller starts or stops the server. Results from an older server instance are discarded by generation number.

Update policy, checked every few seconds while an update is pending:

1. Server down or still booting: apply now.
2. No human players: apply now.
3. Inside the restart window: warn players, then apply.
4. The update was announced (Steam news or the server console): warn, then apply.
5. Pending longer than `max_defer`: warn, then apply.

An announcement stays "urgent" for 30 minutes. During that time SteamCMD is re-checked every 30 seconds, because Steam's app info can lag the news. If no new build appears, the maintainer goes back to regular checks.

## Phase 6b: Keep mods loaded (`gameinfo.gi`)

- Before every server start (first start, crash restart, after an update), make sure each `gameinfo_search_paths` entry is a `Game` line inside `SearchPaths` in `game/csgo/gameinfo.gi`.
- Insert missing lines right after `Game_LowViolence`, or before the first `Game` line if that's missing, so they mount before the stock `Game csgo`. Keep the file's indentation and line endings. Recognize existing lines regardless of quotes, slashes or case. Rewrite the file (atomically) only when something was missing.
- Patching at start rather than after `app_update` also covers manual `validate` runs and updates done outside the maintainer.
- If the file can't be patched (missing, no `SearchPaths`), start anyway and alert admins once that plugins won't load.

## Phase 6c: Discord admin bot (optional)

- Enabled by `discord_bot_token`, and only with `server_manager_role_id`, so a token can't be configured without a role check. Uses `discordgo` (Gateway WebSocket), the project's only dependency.
- Registers four guild-only slash commands. Members without the role get an ephemeral denial. Every use is logged and alerted with the user's name.
- `/restart`: countdown, stop, start, and installs a pending update in the same restart. It also starts the server after `/stop`.
- `/stop`: countdown, then stop. It sets `stopped_by_admin` in the state file, so the server stays down until `/restart`, even across maintainer restarts. Crash restarts, updates and the daily restart leave it alone.
- `/checkupdate`: runs a SteamCMD check now, or joins one already running. It replies with the result; the install itself follows the normal policy.
- `/health`: state (online, starting, not responding, restarting, stopped), map, players, uptime, build, pending update, last check.
- Each command runs as a request on the controller goroutine. Replies are deferred, because Discord drops interactions not answered within 3s. A bad token fails fast with an alert; network errors retry with backoff.

## Phase 7: News as an update trigger, Discord for admins

- Poll GetNewsForApp (`steam_community_announcements`) every `news.interval` (2m).
- Take the newest post whose title contains `news.title` ("Counter-Strike 2 Update"). If it is new (GID and date not seen before) and its `date` is within `news.max_age` (6h) of now, hint the controller to update now. Older posts are recorded but don't trigger anything. The regular SteamCMD poll still catches updates missed while the maintainer was down.
- The Discord webhook (`discord_webhook`) is for admins only:
  - Maintainer started, stopped, or failed. Fatal startup errors and panics are sent synchronously before exit.
  - Server crashed (first crash, then every 5th in a loop), unresponsive, or back up.
  - Update announced, available, restarting for an update, installed, or failed. Also scheduled restarts.
- Discord delivery uses embeds with `allowed_mentions` disabled, handles `429` and `Retry-After`, and goes through an async queue so a slow Discord never blocks the controller.

## Phase 8: Cross-platform build

```
GOOS=linux   GOARCH=amd64 go build -o dist/cs2-maintainer .
GOOS=windows GOARCH=amd64 go build -o dist/cs2-maintainer.exe .
```

## Phase 9: Deployment

- **Linux:** `deploy/cs2-maintainer.service`. Set `TimeoutStopSec` above the server's `stop_timeout`.
- **Windows:** NSSM. It sends Ctrl+C on stop, which the tool handles.
- Open the game port over UDP and TCP. RCON uses the same TCP port, so firewall TCP from outside if RCON isn't needed remotely.

## Phase 10: Testing and hardening

- Tests live in `tests/` and cover:
  - Config, state and VDF parsing.
  - A2S (fake UDP server with a challenge) and RCON (fake TCP server).
  - The news trigger, using the real "Counter-Strike 2 Update" post format, and Discord retries.
  - Log rotation.
  - Controller policy with a fake server, updater and webhook.
  - A real child process (the test binary acting as CS2) for launch, health and clean stop.
- **Failure drills:** kill the server, block the port, corrupt `state.json`, point `steamcmd_path` at nothing, set `build_id` in the manifest to `1`.
- **Known pitfalls:** SteamCMD self-updates on first run, so the timeouts are generous. Windows Defender can quarantine files. Steam endpoints can be slow, so every network call has a timeout.

## Steam API reference

- News: `https://api.steampowered.com/ISteamNews/GetNewsForApp/v2/?appid=730&count=10&maxlength=1&format=json&feeds=steam_community_announcements`
- Steamworks Web API docs: https://partner.steamgames.com/doc/webapi
