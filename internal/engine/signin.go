package engine

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"whydah/internal/config"
	"whydah/internal/events"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

// Some sites don't keep you signed in with borrowed cookies: the session is
// tied to the browser it was made in, or kept in the page's storage rather
// than in a cookie. For those, Whydah has a browser profile of its own that
// people sign in to sites with (Settings → Sign in to sites…), in a window of
// their installed Chrome-family browser. With it chosen for cookies, the
// crow's nest runs on that profile, so a look sees what the window saw, and
// yt-dlp and gallery-dl get its cookies as a cookies.txt.

// cookiesFresh is how long cookies read from the sign-in profile are reused,
// so a playlist's downloads don't each ask the browser.
const cookiesFresh = 30 * time.Second

// SignInStatus is what the UI shows about the sign-in browser.
type SignInStatus struct {
	Browser string `json:"browser"` // what the window opens in, or "" if there's nothing to use
	Open    bool   `json:"open"`    // the window is open
	Profile bool   `json:"profile"` // a profile exists, so some sites may be signed in to
}

// signInCookies caches the sign-in profile's cookies.
type signInCookies struct {
	mu   sync.Mutex
	list []cdpCookie
	at   time.Time
}

func (c *signInCookies) drop() {
	c.mu.Lock()
	c.list, c.at = nil, time.Time{}
	c.mu.Unlock()
}

// signInProfile is the sign-in browser's profile folder, or "" if there's no app folder.
func signInProfile() string {
	dir, err := config.AppDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, tools.SignInProfileName)
}

func profileExists(profile string) bool {
	st, err := os.Stat(profile)
	return profile != "" && err == nil && st.IsDir()
}

func (e *Engine) SignInStatus() SignInStatus {
	e.browsers.mu.Lock()
	open := e.browsers.signingIn && e.browsers.signin != nil
	e.browsers.mu.Unlock()
	return SignInStatus{
		Browser: e.tools.VisibleBrowserName(),
		Open:    open,
		Profile: profileExists(signInProfile()),
	}
}

func (e *Engine) publishSignIn() { e.hub.Publish(events.SignInStatus, e.SignInStatus()) }

// SignIn opens the sign-in window at link (or the browser's new tab page),
// or a new tab of it if it's already open.
func (e *Engine) SignIn(ctx context.Context, link string) error {
	path, profile := e.tools.VisibleBrowser(), signInProfile()
	if path == "" || profile == "" {
		return i18n.New("error.signin_no_browser")
	}
	if link != "" {
		if u, err := url.Parse(link); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			link = ""
		}
	}
	err := e.browsers.openSignIn(ctx, path, profile, link, func() {
		e.signInCookies.drop()
		e.publishSignIn()
	})
	if err != nil {
		if _, ok := err.(*i18n.Error); !ok {
			err = i18n.New("error.signin_start").WithDetail(err.Error())
		}
		return err
	}
	e.signInCookies.drop()
	e.publishSignIn()
	return nil
}

// SignOut deletes the sign-in profile, signing out of every site at once.
func (e *Engine) SignOut() error {
	profile := signInProfile()
	if !profileExists(profile) {
		return nil
	}
	if err := e.browsers.forget(profile); err != nil {
		return err
	}
	e.signInCookies.drop()
	slog.Info("signed out of the sign-in browser")
	e.publishSignIn()
	return nil
}

// usesSignIn reports whether looks and downloads use the sign-in profile:
// it's chosen for cookies, and there's one to use.
func (e *Engine) usesSignIn(s config.Settings) bool {
	return s.BrowserCookies == config.OwnBrowser && e.tools.VisibleBrowser() != "" && profileExists(signInProfile())
}

// signedInCookies reads the sign-in profile's cookies, or reuses ones read
// in the last cookiesFresh.
func (e *Engine) signedInCookies(ctx context.Context) (cookieJar, error) {
	c := &e.signInCookies
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list == nil || time.Since(c.at) > cookiesFresh {
		list, err := e.browsers.cookies(ctx, e.tools.VisibleBrowser(), signInProfile())
		if err != nil {
			return nil, err
		}
		c.list, c.at = list, time.Now()
	}
	return fromCDP(c.list), nil
}

// siteJar is the cookies to send to pageURL's site with the chosen cookie
// source, for requests Whydah makes itself.
func (e *Engine) siteJar(ctx context.Context, paths tools.Paths, s config.Settings, pageURL string) (cookieJar, error) {
	switch {
	case s.BrowserCookies == config.OwnBrowser:
		if !e.usesSignIn(s) {
			return nil, nil
		}
		u, err := url.Parse(pageURL)
		if err != nil {
			return nil, err
		}
		jar, err := e.signedInCookies(ctx)
		return jar.forSite(site(u.Hostname())), err
	case s.BrowserCookies != "":
		return siteCookies(ctx, paths.YtDlp, s.BrowserCookies, pageURL)
	}
	return nil, nil
}

// cookieFile writes the sign-in profile's cookies to a cookies.txt for
// yt-dlp or gallery-dl, when they're the chosen cookies; otherwise it
// returns "". done deletes the file.
func (e *Engine) cookieFile(ctx context.Context, s config.Settings) (file string, done func()) {
	done = func() {}
	if !e.usesSignIn(s) {
		return "", done
	}
	jar, err := e.signedInCookies(ctx)
	if err != nil {
		slog.Warn("couldn't read the sign-in browser's cookies", "err", err)
		return "", done
	}
	dir, err := os.MkdirTemp("", "whydah-cookies-") // private to this user
	if err != nil {
		slog.Warn("couldn't write the sign-in browser's cookies", "err", err)
		return "", done
	}
	file = filepath.Join(dir, "cookies.txt")
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		err = jar.write(f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		slog.Warn("couldn't write the sign-in browser's cookies", "err", err)
		return "", done
	}
	return file, func() { os.RemoveAll(dir) }
}

// FetchImage gets an image for the UI to show, with the sign-in profile's
// cookies and page as the Referer: the UI's own browser isn't signed in to
// the site the sign-in browser found it on. It's only for http(s) links, and
// only while the sign-in browser is the cookie source.
func (e *Engine) FetchImage(ctx context.Context, link, page string) (*http.Response, error) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("not a web link")
	}
	if !e.usesSignIn(e.settings.Get()) {
		return nil, errors.New("the sign-in browser isn't the cookie source")
	}
	jar, err := e.signedInCookies(ctx)
	if err != nil {
		return nil, err
	}
	header := http.Header{"Accept": {"image/avif,image/webp,image/*;q=0.9"}}
	jar.apply(header, u)
	return get(ctx, link, page, header)
}
