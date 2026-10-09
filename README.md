# 🎮 CS2 Server Maintainer

[![Status](https://img.shields.io/badge/Status-Work%20in%20Progress-orange)](https://github.com/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

An automated management and maintenance utility for **Counter-Strike 2** dedicated servers. Designed to keep your CS2 server updated, healthy, and running smoothly without manual intervention.

---

## 🚧 Status: Work in Progress

> **Note**: This project is actively under development. Features, setup procedures, and configurations are subject to rapid change. 

---

## ✨ Features & Capabilities

- 🔄 **Auto-Updates**: Detects new CS2 builds in three ways: a fresh "Counter-Strike 2 Update" post on Steam news, the server's own "restart required" notice, and regular SteamCMD checks. Announced updates install as soon as SteamCMD has the build. Otherwise updates wait for an empty server, your restart window, or a maximum wait. Players get an in-game countdown first.
- 🔁 **Auto-Restarts**: Crash recovery with exponential backoff, plus an optional daily restart.
- 📊 **Health Monitoring**: A2S queries catch a hung server, not just a dead one. Bots don't count as players.
- 🧩 **Keeps Metamod loaded**: CS2 updates replace `gameinfo.gi` and drop lines like `Game csgo/addons/metamod`. Before every start, the maintainer re-adds the search paths you list.
- 🚨 **Admin Alerts**: A Discord webhook tells admins when the maintainer fails, the server crashes or restarts, and when an update is announced, installed or fails.
- 🧱 **Safe by default**: Clean shutdown (SIGINT on Linux, RCON `quit` on Windows). The server can't outlive the maintainer. A lock stops a second maintainer from running. State is written atomically and logs rotate.
- 🛠 **Configurable Hooks** *(planned)*: Run custom scripts before and after an update.

---

## 🛠 Prerequisites

Before installing, ensure you have the following installed on your host system:

- **Operating System**: Linux (Ubuntu 20.04+ / Debian recommended) or Windows Server
- **SteamCMD** installed and configured

---

## 🚀 Quick Start

Requires Go 1.22+ to build. The tool is a single binary with no dependencies.

```bash
cd maintainer
cp config.example.json config.json   # then edit paths, args, rcon_password, gslt
go build -o cs2-maintainer .
./cs2-maintainer -validate           # checks the config and prints the launch command
./cs2-maintainer -check-update       # installed vs. latest build, without touching anything
./cs2-maintainer                     # run it
```

If `install_dir` has no CS2 install yet, the first run installs it with SteamCMD.

Cross-compile from any OS:

```bash
GOOS=linux   GOARCH=amd64 go build -o dist/cs2-maintainer .
GOOS=windows GOARCH=amd64 go build -o dist/cs2-maintainer.exe .
```

---

## ⚙️ Configuration

See [`maintainer/config.example.json`](maintainer/config.example.json) for every option, its default, and a comment explaining it. Config files may contain `//` and `/* */` comments. Only `install_dir` and `steamcmd_path` are required. Durations are strings such as `"15s"` or `"10m"`. Relative paths resolve against the config file's folder. Unknown keys are rejected, so typos fail loudly.

| Key | What it does |
|---|---|
| `server.args` | Extra launch args (map, game mode, ...). `-dedicated`, `-port`, `-usercon`, the RCON password and the GSLT are added for you. |
| `server.rcon_password` | Used for clean shutdown on Windows and for countdown messages. Strongly recommended. |
| `server.console_window` | Windows: the server gets its own console window, like a `.bat` (default). You see its output and can type commands there. Set to `false` to capture its output into `logs/server.log` instead. |
| `update.window` | Daily window (`start`/`end`, local time, may cross midnight) when updates install even with players online. |
| `update.max_defer` | Longest an update waits for an empty server. After an update, outdated servers reject updated clients, so don't wait forever. `"0s"` = no limit. |
| `update.validate` | Validate all files on every update. Off by default because validation is slow; it always runs after a failed update. |
| `restart.daily_at` | Optional daily restart time, for example `"05:00"`. |
| `discord_webhook` | Discord webhook for admin alerts: maintainer failures, server restarts, updates. |
| `gameinfo_search_paths` | Search paths to keep in `game/csgo/gameinfo.gi`, for example `["csgo/addons/metamod"]`. Each is re-added as a `Game` line after `Game_LowViolence` before every server start, since updates reset the file. |
| `news.title` / `news.max_age` | A Steam news post whose title contains `title` (default `"Counter-Strike 2 Update"`) and that is newer than `max_age` (default `6h`) triggers an update. |

Secrets can come from the environment instead: `CS2M_RCON_PASSWORD`, `CS2M_GSLT`, `CS2M_DISCORD_WEBHOOK`.

Logs go to `logs/maintainer.log` (the tool) and `logs/server.log` (CS2 console). Both rotate.

---

## 📦 Deployment

**Linux (systemd):** copy [`maintainer/deploy/cs2-maintainer.service`](maintainer/deploy/cs2-maintainer.service) to `/etc/systemd/system/`, adjust `User` and paths, then run `systemctl enable --now cs2-maintainer`. The tool also copies `steamclient.so` into `~/.steam/sdk64/` for you, which is the most common reason a Linux CS2 server fails to start.

**Windows:** run it as a service with [NSSM](https://nssm.cc/):

```powershell
nssm install CS2Maintainer C:\cs2-maintainer\cs2-maintainer.exe -config C:\cs2-maintainer\config.json
nssm set CS2Maintainer AppDirectory C:\cs2-maintainer
nssm start CS2Maintainer
```

Set `server.rcon_password`, otherwise Windows can only force-kill the server. Add an antivirus exclusion for the CS2 and SteamCMD folders.

**Firewall:** open the game port (default 27015) for UDP. RCON shares the port over TCP, so only open TCP if you need RCON from outside.

---

## 🧪 Development

```
maintainer/
├── main.go     flags and wiring
├── core/       all logic (controller, supervisor, updater, health, rcon, news, ...)
├── tests/      test suite
└── deploy/     systemd unit
```

```bash
cd maintainer
go test ./...              # or: go test ./tests/ -v
```

The tests include fake A2S and RCON servers, a fake Steam news API and Discord webhook, and the controller's policies (crash backoff, hang detection, update deferral, news-triggered updates). One test launches the test binary itself as a stand-in CS2 server to exercise the real start, health and stop path.

See [`maintainer/plan.md`](maintainer/plan.md) for the design and the reasoning behind it.
