package tests

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cs2-maintainer/core"
)

// The real post from the user's example.
const exampleUpdateDate = 1791499009

func TestLatestUpdatePost(t *testing.T) {
	items := []core.NewsItem{
		{GID: "a", Title: "Premier Season Three", Date: exampleUpdateDate + 500},
		{GID: "b", Title: "Counter-Strike 2 Update", Date: exampleUpdateDate},
		{GID: "c", Title: "counter-strike 2 update", Date: exampleUpdateDate - 86400},
	}
	it, ok := core.LatestUpdatePost(items, "Counter-Strike 2 Update")
	if !ok || it.GID != "b" {
		t.Errorf("got %+v, %v; want the newest matching post, not the newer non-update one", it, ok)
	}
	if _, ok := core.LatestUpdatePost(items[:1], "Counter-Strike 2 Update"); ok {
		t.Error("matched a post that isn't an update")
	}
}

func TestIsRecent(t *testing.T) {
	posted := time.Unix(exampleUpdateDate, 0)
	cases := []struct {
		after time.Duration
		want  bool
	}{
		{30 * time.Minute, true},
		{6 * time.Hour, true},
		{6*time.Hour + time.Second, false},
		{48 * time.Hour, false},
		{-5 * time.Minute, true}, // small clock skew
		{-2 * time.Hour, false},  // far in the future
	}
	for _, c := range cases {
		if got := core.IsRecent(posted, posted.Add(c.after), 6*time.Hour); got != c.want {
			t.Errorf("now = posted%+v: IsRecent = %v, want %v", c.after, got, c.want)
		}
	}
}

type newsHarness struct {
	mu      sync.Mutex
	feed    string
	updates []core.NewsItem
	watcher *core.NewsWatcher
	state   *core.StateStore
}

func newNewsHarness(t *testing.T, now time.Time) *newsHarness {
	h := &newsHarness{}
	steam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("appid") != "730" || q.Get("feeds") != "steam_community_announcements" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		io.WriteString(w, h.feed)
	}))
	t.Cleanup(steam.Close)
	st, err := core.OpenState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.state = st
	h.watcher = core.NewNewsWatcher(core.DefaultConfig(), discardLogger(), st, func(it core.NewsItem) {
		h.mu.Lock()
		h.updates = append(h.updates, it)
		h.mu.Unlock()
	})
	h.watcher.Endpoint = steam.URL
	h.watcher.Now = func() time.Time { return now }
	return h
}

func (h *newsHarness) poll(t *testing.T, feed string) int {
	t.Helper()
	h.mu.Lock()
	h.feed = feed
	h.mu.Unlock()
	if err := h.watcher.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.updates)
}

const exampleFeed = `{"appnews":{"appid":730,"newsitems":[
	{"gid":"5822","title":"Counter-Strike 2 Update","url":"https://store.steampowered.com/news/app/730/view/5822","date":1791499009},
	{"gid":"5800","title":"Counter-Strike 2 Update","date":1791300000}]}}`

func TestNewsTriggersUpdateForRecentPost(t *testing.T) {
	h := newNewsHarness(t, time.Unix(exampleUpdateDate, 0).Add(20*time.Minute))
	if n := h.poll(t, exampleFeed); n != 1 {
		t.Fatalf("update triggered %d times, want 1", n)
	}
	if h.updates[0].GID != "5822" {
		t.Errorf("triggered for %+v", h.updates[0])
	}
	if n := h.poll(t, exampleFeed); n != 1 {
		t.Errorf("the same post triggered again (%d triggers)", n)
	}
	if h.state.Get().NewsGID != "5822" {
		t.Errorf("handled post not saved: %+v", h.state.Get())
	}
}

func TestNewsIgnoresOldPost(t *testing.T) {
	h := newNewsHarness(t, time.Unix(exampleUpdateDate, 0).Add(2*24*time.Hour))
	if n := h.poll(t, exampleFeed); n != 0 {
		t.Errorf("a two-day-old post triggered an update")
	}
	if h.state.Get().NewsGID != "5822" {
		t.Error("old post should still be recorded so it's never reconsidered")
	}
}

func TestNewsIgnoresOtherAnnouncements(t *testing.T) {
	h := newNewsHarness(t, time.Unix(exampleUpdateDate, 0).Add(time.Minute))
	feed := `{"appnews":{"newsitems":[{"gid":"9","title":"The Armory is open","date":1791499000}]}}`
	if n := h.poll(t, feed); n != 0 {
		t.Error("a non-update announcement triggered an update")
	}
}
