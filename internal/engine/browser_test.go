package engine

import (
	"context"
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

	ha, sa, doneA, err := b.tab(ctx, path, newPageWatch())
	if err != nil {
		t.Fatal(err)
	}
	hb, sb, doneB, err := b.tab(ctx, path, newPageWatch())
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
	hc, _, doneC, err := b.tab(ctx, path, newPageWatch())
	if err != nil {
		t.Fatal(err)
	}
	defer doneC()
	if hc == ha {
		t.Error("kept using a browser that had exited")
	}
}
