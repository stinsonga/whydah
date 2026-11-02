// Package clipboard polls the system clipboard and publishes supported URLs.
package clipboard

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"whydah/internal/config"
	"whydah/internal/events"
)

const pollInterval = time.Second

// Watch polls until ctx is done. Whatever is on the clipboard at startup is
// ignored, and each URL is only reported once per session.
func Watch(ctx context.Context, settings *config.Store, hub *events.Hub) {
	last, err := read()
	if err != nil {
		slog.Warn("clipboard unavailable; watcher disabled", "err", err)
		return
	}
	seen := map[string]bool{strings.TrimSpace(last): true}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s := settings.Get()
		if !s.WatchClipboard {
			continue
		}
		text, err := read()
		if err != nil || text == last {
			continue
		}
		last = text
		url := strings.TrimSpace(text)
		if seen[url] || !config.IsSupportedURL(url, s.SupportedHosts) {
			continue
		}
		seen[url] = true
		slog.Info("clipboard url", "url", url)
		hub.Publish(events.URLDetected, events.URLDetectedData{URL: url})
	}
}
