package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"whydah/internal/events"
	"whydah/internal/tools"
)

// TestBrowserPool runs a real browser, so it's skipped where there's none.
func TestBrowserPool(t *testing.T) {
	tm := tools.New(t.TempDir(), events.NewHub(), func() string { return "stable" })
	if !tm.BrowserReady() {
		t.Skip("no Chromium-family browser installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path, err := tm.Browser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var b browserPool
	defer b.close()

	ha, sa, doneA, err := b.tab(ctx, path, "", newPageWatch())
	if err != nil {
		t.Fatal(err)
	}
	hb, sb, doneB, err := b.tab(ctx, path, "", newPageWatch())
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatal("two looks started two browsers")
	}

	// A cookie set in one look's tab mustn't show up in the other's.
	site := "https://whydah.test/"
	if err := ha.cdp.call(ctx, sa, "Network.setCookies", map[string]any{"cookies": []map[string]any{{"name": "k", "value": "v", "url": site}}}, nil); err != nil {
		t.Fatal(err)
	}
	count := func(session string) int {
		var r struct{ Cookies []struct{ Name string } }
		if err := ha.cdp.call(ctx, session, "Network.getCookies", map[string]any{"urls": []string{site}}, &r); err != nil {
			t.Fatal(err)
		}
		return len(r.Cookies)
	}
	if a, b := count(sa), count(sb); a != 1 || b != 0 {
		t.Errorf("cookies: %d in the look that set it, %d in the other; want 1 and 0", a, b)
	}

	doneA()
	doneB()
	doneB() // twice is fine
	b.mu.Lock()
	users, running := b.users, b.h != nil
	b.mu.Unlock()
	if users != 0 || !running {
		t.Errorf("after both looks: %d users, running %v; want 0 and still running until idle", users, running)
	}

	// A browser that died is replaced on the next look.
	ha.kill()
	deadline := time.Now().Add(10 * time.Second)
	for !ha.exited() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	hc, _, doneC, err := b.tab(ctx, path, "", newPageWatch())
	if err != nil {
		t.Fatal(err)
	}
	defer doneC()
	if hc == ha {
		t.Error("kept using a browser that had exited")
	}
}

// TestSignInProfile runs a real browser on a kept profile, so it's skipped
// where there's no installed one.
func TestSignInProfile(t *testing.T) {
	tm := tools.New(t.TempDir(), events.NewHub(), func() string { return "stable" })
	path := tm.VisibleBrowser()
	if path == "" {
		t.Skip("no Chromium-family browser installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	profile := filepath.Join(t.TempDir(), tools.SignInProfileName)
	var b browserPool
	defer b.close()

	h, session, done, err := b.tab(ctx, path, profile, newPageWatch())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Unix()
	if err := h.cdp.call(ctx, session, "Network.setCookies", map[string]any{"cookies": []map[string]any{
		{"name": "k", "value": "v", "url": "https://whydah.test/", "expires": expires}}}, nil); err != nil {
		t.Fatal(err)
	}
	done()
	b.shut(true)

	// The cookie outlives the browser, and the next look sees it.
	list, err := b.cookies(ctx, path, profile)
	if err != nil {
		t.Fatal(err)
	}
	jar := fromCDP(list).forSite("whydah.test")
	if len(jar) != 1 || jar[0].Value != "v" || jar[0].Expires != expires {
		t.Errorf("cookies after a restart: %+v; want k=v expiring %d", jar, expires)
	}

	b.mu.Lock()
	b.hold() // as while the sign-in window is open
	b.mu.Unlock()
	if _, _, _, err := b.tab(ctx, path, profile, newPageWatch()); err == nil {
		t.Error("a look used the profile while the sign-in window had it")
	}
	if _, _, done, err := b.tab(ctx, path, "", newPageWatch()); err != nil {
		t.Errorf("a look without the profile: %v", err)
	} else {
		done()
	}
	short, cancelShort := context.WithTimeout(ctx, 100*time.Millisecond)
	if err := b.waitSignIn(short); err == nil {
		t.Error("waitSignIn returned while the window was open")
	}
	cancelShort()
	waited := make(chan error, 1)
	go func() { waited <- b.waitSignIn(ctx) }()
	b.mu.Lock()
	b.unhold() // the window closes
	b.mu.Unlock()
	if err := <-waited; err != nil {
		t.Errorf("waitSignIn after the window closed: %v", err)
	}

	if err := b.forget(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Errorf("the profile is still there after forgetting it: %v", err)
	}
}
