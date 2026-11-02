package engine

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"whydah/internal/i18n"
)

// The crow's nest keeps one browser running between looks, since starting
// one takes a second or two. Each look gets a tab in a browser context of
// its own (like a private window), so one page's cookies and storage never
// reach the next. The browser quits after browserIdle without a look, when
// Whydah quits, and before a refit (which may replace it).
//
// With the sign-in browser chosen for cookies (signin.go), the browser runs
// on the sign-in profile instead, and looks share its cookies and storage.
// Chrome runs one browser per profile, so while the sign-in window is open
// the pool leaves the profile to it, and looks on it wait.

const browserIdle = 2 * time.Minute

type browserPool struct {
	mu       sync.Mutex
	h        *headless
	path     string
	profile  string // h's kept profile, or "" for a throwaway one
	users    int
	idle     *time.Timer
	watchers map[*pageWatch]bool

	signingIn bool          // the sign-in window has the profile, or is about to (or it's being deleted)
	signin    *headless     // the sign-in window's browser, once started
	released  chan struct{} // closed when signingIn ends
}

// hold keeps looks off the sign-in profile until unhold. b.mu must be held.
func (b *browserPool) hold() {
	b.signingIn = true
	b.released = make(chan struct{})
}

// unhold lets looks use the sign-in profile again. b.mu must be held.
func (b *browserPool) unhold() {
	if b.signingIn {
		b.signingIn = false
		close(b.released)
	}
}

// waitSignIn waits until nothing holds the sign-in profile.
func (b *browserPool) waitSignIn(ctx context.Context) error {
	b.mu.Lock()
	held, released := b.signingIn, b.released
	b.mu.Unlock()
	if !held {
		return nil
	}
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// acquire returns the browser at path on profile ("" for a throwaway one),
// starting it if it isn't running. release must be called when done.
func (b *browserPool) acquire(ctx context.Context, path, profile string) (h *headless, release func(), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if profile != "" && b.signingIn {
		return nil, nil, i18n.New("error.signin_open")
	}
	if b.idle != nil {
		b.idle.Stop()
	}
	if b.h != nil && (b.path != path || b.profile != profile || b.h.exited()) {
		old := b.h
		b.h = nil
		go old.close()
	}
	if b.h == nil {
		started := time.Now()
		if b.h, err = startBrowser(ctx, path, browserOpts{profile: profile}, b.dispatch); err != nil {
			return nil, nil, err
		}
		b.path, b.profile = path, profile
		slog.Info("crow's nest browser started", "browser", path, "signed in", profile != "", "took", time.Since(started).Round(10*time.Millisecond))
	}
	h = b.h
	b.users++
	var once sync.Once
	return h, func() {
		once.Do(func() {
			b.mu.Lock()
			if b.users--; b.users == 0 && b.h == h {
				b.idle = time.AfterFunc(browserIdle, b.closeIdle)
			}
			b.mu.Unlock()
		})
	}, nil
}

// tab opens a tab in the browser at path on profile, starting the browser if
// it isn't running, and sends the tab's events to w. On a throwaway profile
// the tab gets a browser context of its own. done closes the tab (and its
// context); it's safe to call more than once.
func (b *browserPool) tab(ctx context.Context, path, profile string, w *pageWatch) (h *headless, session string, done func(), err error) {
	h, release, err := b.acquire(ctx, path, profile)
	if err != nil {
		return nil, "", nil, err
	}
	b.mu.Lock()
	if b.watchers == nil {
		b.watchers = map[*pageWatch]bool{}
	}
	b.watchers[w] = true
	b.mu.Unlock()

	var browserContext string
	var once sync.Once
	done = func() {
		once.Do(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if browserContext != "" {
				_ = h.cdp.call(cctx, "", "Target.disposeBrowserContext", map[string]any{"browserContextId": browserContext}, nil)
			} else if session != "" {
				_ = h.cdp.call(cctx, session, "Page.close", nil, nil)
			}
			cancel()
			b.mu.Lock()
			delete(b.watchers, w)
			b.mu.Unlock()
			release()
		})
	}
	if profile == "" {
		browserContext, err = h.newContext(ctx)
	}
	if err == nil {
		session, err = h.newPage(ctx, browserContext)
	}
	if err != nil {
		done()
		return nil, "", nil, err
	}
	return h, session, done, nil
}

// cdpCookie is a cookie as Storage.getCookies reports it.
type cdpCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"` // with a leading dot when sent to subdomains
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // Unix seconds; -1 for a session cookie
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
}

// cookies reads every cookie in profile: from the sign-in window if it's
// open, otherwise from the headless browser on it.
func (b *browserPool) cookies(ctx context.Context, path, profile string) ([]cdpCookie, error) {
	b.mu.Lock()
	h := b.signin
	b.mu.Unlock()
	if h == nil {
		if err := b.waitSignIn(ctx); err != nil { // the window is starting, or the profile is being deleted
			return nil, err
		}
		var release func()
		var err error
		if h, release, err = b.acquire(ctx, path, profile); err != nil {
			return nil, err
		}
		defer release()
	}
	var r struct {
		Cookies []cdpCookie `json:"cookies"`
	}
	err := h.cdp.call(ctx, "", "Storage.getCookies", nil, &r)
	return r.Cookies, err
}

// openSignIn opens a window of the browser at path on profile, at url (or
// its new tab page). If the window is already open, url opens in a new tab
// of it. ended is called once the window has been closed.
func (b *browserPool) openSignIn(ctx context.Context, path, profile, url string, ended func()) error {
	b.mu.Lock()
	if b.signingIn {
		h := b.signin
		b.mu.Unlock()
		if h == nil {
			return nil // still starting
		}
		return h.cdp.call(ctx, "", "Target.createTarget", map[string]any{"url": firstNonEmpty(url, "chrome://newtab/")}, nil)
	}
	var old *headless
	if b.h != nil && b.profile == profile {
		if b.users > 0 {
			b.mu.Unlock()
			return i18n.New("error.signin_busy")
		}
		old, b.h = b.h, nil
	}
	b.hold()
	b.mu.Unlock()

	if old != nil {
		old.close() // it must let go of the profile first
	}
	h, err := startBrowser(ctx, path, browserOpts{profile: profile, visible: true, url: url}, nil)
	b.mu.Lock()
	if err != nil {
		b.unhold()
		b.mu.Unlock()
		return err
	}
	b.signin = h
	b.mu.Unlock()
	slog.Info("sign-in browser opened", "browser", path)
	go b.watchSignIn(h, ended)
	return nil
}

// watchSignIn waits for the sign-in window to be closed, then quits its
// browser: on macOS a browser keeps running with no windows open.
func (b *browserPool) watchSignIn(h *headless, ended func()) {
	seen := false // a page; there may be none for a moment at the start
	for !h.exited() {
		time.Sleep(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var r struct {
			TargetInfos []struct {
				Type string `json:"type"`
			} `json:"targetInfos"`
		}
		err := h.cdp.call(ctx, "", "Target.getTargets", nil, &r)
		cancel()
		if err != nil {
			continue
		}
		pages := 0
		for _, t := range r.TargetInfos {
			if t.Type == "page" {
				pages++
			}
		}
		if pages > 0 {
			seen = true
		} else if seen {
			break
		}
	}
	h.close()
	b.mu.Lock()
	b.signin = nil
	b.unhold()
	b.mu.Unlock()
	slog.Info("sign-in browser closed")
	ended()
}

// closeSignIn quits the sign-in window's browser, for quitting Whydah.
func (b *browserPool) closeSignIn() {
	b.mu.Lock()
	h := b.signin
	b.mu.Unlock()
	if h != nil {
		h.close() // watchSignIn sees it go and tidies up
	}
}

// forget deletes profile, quitting the headless browser on it first.
func (b *browserPool) forget(profile string) error {
	b.mu.Lock()
	if b.signingIn {
		b.mu.Unlock()
		return i18n.New("error.signin_open")
	}
	var old *headless
	if b.h != nil && b.profile == profile {
		if b.users > 0 {
			b.mu.Unlock()
			return i18n.New("error.signin_busy")
		}
		old, b.h = b.h, nil
	}
	b.hold() // keep looks off the profile while it goes
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.unhold()
		b.mu.Unlock()
	}()
	if old != nil {
		old.close()
	}
	var err error
	for range 5 { // its helper processes may still be letting go of files
		if err = os.RemoveAll(profile); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// closeIdle quits the browser if no look is using it.
func (b *browserPool) closeIdle() { b.shut(false) }

// close quits the browser even mid-look, for quitting Whydah.
func (b *browserPool) close() {
	b.shut(true)
	b.closeSignIn()
}

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
