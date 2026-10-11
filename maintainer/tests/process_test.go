package tests

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cs2-maintainer/core"
)

// TestLaunchServerHealthAndCleanStop runs the test binary itself as a fake CS2
// server (see TestMain) through the real launch, A2S and stop code. On Windows
// the stop goes through RCON "quit"; on Linux through SIGINT.
func TestLaunchServerHealthAndCleanStop(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS2M_FAKE_SERVER", "1")
	if err := core.InitProcessJob(); err != nil {
		t.Fatal(err)
	}

	cfg := core.DefaultConfig()
	cfg.Server.Binary = exe
	cfg.Server.Port = freePort(t)
	cfg.Server.RCONPassword = "pw"
	cfg.Server.ConsoleWindow = false // capture output so it can be checked

	var mu sync.Mutex
	var lines []string
	p, err := core.LaunchServer(cfg, discardLogger(), func(l string) {
		mu.Lock()
		lines = append(lines, l)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}

	var info core.A2SInfo
	waitFor(t, "fake server to answer A2S", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		info, err = core.QueryA2SInfo(ctx, cfg.QueryAddr())
		return err == nil
	})
	if info.Map != "de_mirage" {
		t.Errorf("unexpected info %+v", info)
	}

	start := time.Now()
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("stop took %s; the graceful path was not used", took)
	}
	if p.Err() != nil {
		t.Errorf("server exit error %v; it should have exited cleanly", p.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "fake server ready on port "+strconv.Itoa(cfg.Server.Port)) ||
		!strings.Contains(joined, "saving and exiting") {
		t.Errorf("console output not captured: %q", joined)
	}
}

// On Windows the server runs in its own console window by default, like a .bat
// file. Output isn't captured then, but health checks and the clean RCON stop
// must work the same.
func TestLaunchServerInOwnConsoleWindow(t *testing.T) {
	cfg := core.DefaultConfig()
	if !cfg.UsesConsoleWindow() {
		t.Skip("separate console windows are Windows-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS2M_FAKE_SERVER", "1")
	if err := core.InitProcessJob(); err != nil {
		t.Fatal(err)
	}
	cfg.Server.Binary = exe
	cfg.Server.Port = freePort(t)
	cfg.Server.RCONPassword = "pw"

	p, err := core.LaunchServer(cfg, discardLogger(), func(l string) {
		t.Errorf("output should go to the server's own window, got %q", l)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake server to answer A2S", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := core.QueryA2SInfo(ctx, cfg.QueryAddr())
		return err == nil
	})
	start := time.Now()
	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("stop took %s; the graceful path was not used", took)
	}
	if p.Err() != nil {
		t.Errorf("exit error %v; it should have exited cleanly", p.Err())
	}
}

func TestIsConsoleNoise(t *testing.T) {
	if !core.IsConsoleNoise("CTextConsoleWin::GetLine: !GetNumberOfConsoleInputEvents") {
		t.Error("the Windows console input spam should be filtered")
	}
	if core.IsConsoleNoise("Loaded map de_mirage") {
		t.Error("filtered a real line")
	}
}

func TestIsUpdateHint(t *testing.T) {
	for line, want := range map[string]bool{
		"MasterRequestRestart": true,
		"Your server needs to be restarted in order to receive the latest update.": true,
		"Loaded map de_mirage": false,
	} {
		if got := core.IsUpdateHint(line); got != want {
			t.Errorf("IsUpdateHint(%q) = %v", line, got)
		}
	}
}
