package core

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const NewsEndpoint = "https://api.steampowered.com/ISteamNews/GetNewsForApp/v2/"

type NewsItem struct {
	GID   string `json:"gid"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Date  int64  `json:"date"`
}

type newsResponse struct {
	AppNews struct {
		NewsItems []NewsItem `json:"newsitems"`
	} `json:"appnews"`
}

// NewsWatcher reads Steam announcements and reports a fresh update post (for
// example "Counter-Strike 2 Update") through onUpdate. It runs on its own so a
// slow or failing news request never delays the controller.
type NewsWatcher struct {
	cfg      *Config
	log      *slog.Logger
	state    *StateStore
	onUpdate func(NewsItem)
	client   *http.Client
	Endpoint string
	Now      func() time.Time
}

func NewNewsWatcher(cfg *Config, log *slog.Logger, state *StateStore, onUpdate func(NewsItem)) *NewsWatcher {
	return &NewsWatcher{
		cfg:      cfg,
		log:      log,
		state:    state,
		onUpdate: onUpdate,
		client:   &http.Client{Timeout: 20 * time.Second},
		Endpoint: NewsEndpoint,
		Now:      time.Now,
	}
}

func (w *NewsWatcher) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.News.Interval.Duration)
	defer t.Stop()
	for {
		if err := w.Poll(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("news check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll fetches the feed once and calls onUpdate if it finds a new, recent update post.
func (w *NewsWatcher) Poll(ctx context.Context) error {
	items, err := w.fetch(ctx)
	if err != nil {
		return err
	}
	st := w.state.Get()
	it, ok := LatestUpdatePost(items, w.cfg.News.Title)
	if !ok || it.GID == st.NewsGID || it.Date <= st.NewsDate {
		return nil // no update post, or we already handled this one
	}
	if err := w.state.Update(func(s *State) { s.NewsGID, s.NewsDate = it.GID, it.Date }); err != nil {
		w.log.Error("saving state", "err", err)
	}
	posted := time.Unix(it.Date, 0)
	if !IsRecent(posted, w.Now(), w.cfg.News.MaxAge.Duration) {
		w.log.Info("ignoring old update post", "title", it.Title, "posted", posted.Format(time.DateTime),
			"max_age", w.cfg.News.MaxAge.Duration)
		return nil
	}
	w.log.Info("Steam announced a CS2 update", "title", it.Title, "posted", posted.Format(time.DateTime), "url", it.URL)
	w.onUpdate(it)
	return nil
}

// LatestUpdatePost returns the newest item whose title contains title,
// ignoring case.
func LatestUpdatePost(items []NewsItem, title string) (NewsItem, bool) {
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(a, b NewsItem) int { return cmp.Compare(b.Date, a.Date) })
	want := strings.ToLower(title)
	for _, it := range sorted {
		if strings.Contains(strings.ToLower(it.Title), want) {
			return it, true
		}
	}
	return NewsItem{}, false
}

// IsRecent reports whether posted is within maxAge before now. Small clock skew
// into the future is allowed.
func IsRecent(posted, now time.Time, maxAge time.Duration) bool {
	age := now.Sub(posted)
	return age <= maxAge && age >= -10*time.Minute
}

func (w *NewsWatcher) fetch(ctx context.Context) ([]NewsItem, error) {
	q := url.Values{
		"appid":     {strconv.Itoa(w.cfg.AppID)},
		"count":     {"10"},
		"maxlength": {"1"}, // only titles and dates are needed
		"format":    {"json"},
		"feeds":     {"steam_community_announcements"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("steam news returned %s", resp.Status)
	}
	var r newsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("decoding steam news: %w", err)
	}
	return r.AppNews.NewsItems, nil
}
