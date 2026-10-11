package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	LevelInfo    = "info"
	LevelSuccess = "success"
	LevelWarn    = "warn"
	LevelError   = "error"
)

type Event struct {
	Level string
	Title string
	Text  string
	URL   string
	Time  time.Time
}

// Discord posts admin alerts to a webhook. A nil *Discord is valid and does nothing,
// so callers never need to check whether a webhook is configured.
type Discord struct {
	url    string
	client *http.Client
	log    *slog.Logger
	queue  chan Event
	done   chan struct{}
}

func NewDiscord(webhook string, log *slog.Logger) *Discord {
	if webhook == "" {
		return nil
	}
	d := &Discord{
		url:    webhook,
		client: &http.Client{Timeout: 15 * time.Second},
		log:    log,
		queue:  make(chan Event, 64),
		done:   make(chan struct{}),
	}
	go d.worker()
	return d
}

// Notify queues ev without blocking. If Discord is slow and the queue is full,
// the event is dropped; it is still in the log.
func (d *Discord) Notify(ev Event) {
	if d == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	select {
	case d.queue <- ev:
	default:
		d.log.Warn("discord queue full; dropping notification", "title", ev.Title)
	}
}

func (d *Discord) worker() {
	defer close(d.done)
	for ev := range d.queue {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := d.Send(ctx, ev); err != nil {
			d.log.Warn("discord notification failed", "title", ev.Title, "err", err)
		}
		cancel()
	}
}

// Close sends what's queued, waiting at most timeout. Notify must not be called after Close.
func (d *Discord) Close(timeout time.Duration) {
	if d == nil {
		return
	}
	close(d.queue)
	select {
	case <-d.done:
	case <-time.After(timeout):
	}
}

// Send posts ev and waits for Discord to accept it, honoring rate limits.
func (d *Discord) Send(ctx context.Context, ev Event) error {
	if d == nil {
		return nil
	}
	body, err := json.Marshal(discordPayload(ev))
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.client.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			if attempt >= 3 || ctx.Err() != nil {
				return err
			}
			wait = time.Duration(attempt) * 2 * time.Second
		default:
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return fmt.Errorf("discord returned %s", resp.Status)
			}
			if attempt >= 4 {
				return fmt.Errorf("discord returned %s after %d attempts", resp.Status, attempt)
			}
			wait = retryAfter(resp.Header.Get("Retry-After"), time.Duration(attempt)*2*time.Second)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(wait):
		}
	}
}

func retryAfter(header string, fallback time.Duration) time.Duration {
	if secs, err := strconv.ParseFloat(header, 64); err == nil && secs >= 0 {
		return min(time.Duration(secs*float64(time.Second))+100*time.Millisecond, time.Minute)
	}
	return fallback
}

type discordEmbed struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Color       int    `json:"color"`
	Timestamp   string `json:"timestamp,omitempty"`
}

var levelColors = map[string]int{
	LevelInfo:    0x3498DB,
	LevelSuccess: 0x2ECC71,
	LevelWarn:    0xF1C40F,
	LevelError:   0xE74C3C,
}

func discordPayload(ev Event) map[string]any {
	embed := discordEmbed{
		Title:       truncate(ev.Title, 256),
		Description: truncate(ev.Text, 4000),
		URL:         ev.URL,
		Color:       levelColors[ev.Level],
	}
	if !ev.Time.IsZero() {
		embed.Timestamp = ev.Time.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"username": "CS2 Maintainer",
		"embeds":   []discordEmbed{embed},
		// Error text can contain anything; never let it ping @everyone.
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
