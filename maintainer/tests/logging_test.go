package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cs2-maintainer/core"
)

func TestLineWriter(t *testing.T) {
	var lines []string
	w := core.NewLineWriter(func(l string) { lines = append(lines, l) })
	io.WriteString(w, "first\r\nsec")
	io.WriteString(w, "ond\n\nthird")
	w.Flush()
	want := []string{"first", "second", "", "third"}
	if !slices.Equal(lines, want) {
		t.Errorf("got %q, want %q", lines, want)
	}
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	r, err := core.OpenRotating(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		io.WriteString(r, s)
	}
	r.Close()
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(path) != "dddddddd\n" || read(path+".1") != "cccccccc\n" || read(path+".2") != "bbbbbbbb\n" {
		t.Errorf("rotation wrong: %q %q %q", read(path), read(path+".1"), read(path+".2"))
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("kept more than max_files backups")
	}
}

func TestBackoff(t *testing.T) {
	var got []time.Duration
	for i := 1; i <= 6; i++ {
		got = append(got, core.Backoff(i, 5*time.Second, time.Minute))
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	if !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestDiscordRetriesRateLimitAndBlocksMentions(t *testing.T) {
	var calls atomic.Int32
	var lastBody atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0.05")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	d := core.NewDiscord(hook.URL, discardLogger())
	defer d.Close(time.Second)
	err := d.Send(context.Background(), core.Event{Level: core.LevelError, Title: "CS2 maintainer failed", Text: "@everyone boom"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected one retry after 429, got %d calls", calls.Load())
	}
	if body := lastBody.Load().(string); !strings.Contains(body, `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("mentions not disabled: %s", body)
	}
}

func TestNilDiscordIsANoOp(t *testing.T) {
	var d *core.Discord = core.NewDiscord("", discardLogger())
	d.Notify(core.Event{Title: "x"})
	if err := d.Send(context.Background(), core.Event{Title: "x"}); err != nil {
		t.Error(err)
	}
	d.Close(time.Second)
}
