package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// The crow's nest keeps one browser running between looks, since starting
// one takes a second or two. Each look gets a tab in a browser context of
// its own (like a private window), so one page's cookies and storage never
// reach the next. The browser quits after browserIdle without a look, when
// Whydah quits, and before a refit (which may replace it).

const browserIdle = 2 * time.Minute

type browserPool struct {
	mu       sync.Mutex
	h        *headless
	path     string
	users    int
	idle     *time.Timer
	watchers map[*pageWatch]bool
}

// dispatch hands a browser event to every look; each keeps its own tab's.
func (b *browserPool) dispatch(m cdpMsg) {
	b.mu.Lock()
	ws := make([]*pageWatch, 0, len(b.watchers))
	for w := range b.watchers {
		ws = append(ws, w)
	}
	b.mu.Unlock()
	for _, w := range ws {
		w.event(m)
	}
}

// tab opens a tab in a new browser context of the browser at path, starting
// the browser if it isn't running, and sends the tab's events to w. done
// closes the tab and its context; it's safe to call more than once.
func (b *browserPool) tab(ctx context.Context, path string, w *pageWatch) (h *headless, session string, done func(), err error) {
	b.mu.Lock()
	if b.idle != nil {
		b.idle.Stop()
	}
	if b.h != nil && (b.path != path || b.h.exited()) {
		old := b.h
		b.h = nil
		go old.close()
	}
	if b.h == nil {
		started := time.Now()
		if b.h, err = startHeadless(ctx, path, b.dispatch); err != nil {
			b.mu.Unlock()
			return nil, "", nil, err
		}
		b.path = path
		slog.Info("crow's nest browser started", "browser", path, "took", time.Since(started).Round(10*time.Millisecond))
	}
	h = b.h
	b.users++
	if b.watchers == nil {
		b.watchers = map[*pageWatch]bool{}
	}
	b.watchers[w] = true
	b.mu.Unlock()

	var browserContext string
	var once sync.Once
	done = func() {
		once.Do(func() {
			if browserContext != "" {
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = h.cdp.call(cctx, "", "Target.disposeBrowserContext", map[string]any{"browserContextId": browserContext}, nil)
				cancel()
			}
			b.mu.Lock()
			delete(b.watchers, w)
			if b.users--; b.users == 0 && b.h == h {
				b.idle = time.AfterFunc(browserIdle, b.closeIdle)
			}
			b.mu.Unlock()
		})
	}
	if browserContext, err = h.newContext(ctx); err == nil {
		session, err = h.newPage(ctx, browserContext)
	}
	if err != nil {
		done()
		return nil, "", nil, err
	}
	return h, session, done, nil
}

// closeIdle quits the browser if no look is using it.
func (b *browserPool) closeIdle() { b.shut(false) }

// close quits the browser even mid-look, for quitting Whydah.
func (b *browserPool) close() { b.shut(true) }

func (b *browserPool) shut(force bool) {
	b.mu.Lock()
	h := b.h
	if h == nil || (b.users > 0 && !force) {
		b.mu.Unlock()
		return
	}
	b.h = nil
	b.mu.Unlock()
	h.close()
	slog.Info("crow's nest browser closed")
}
