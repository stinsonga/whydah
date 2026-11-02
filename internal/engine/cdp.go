package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"whydah/internal/tools"
)

// A minimal Chrome DevTools Protocol client: just enough to start a headless
// browser, load a page in it, scroll it and read what it shows.

const devtoolsOrigin = "http://127.0.0.1" // the only origin the browser accepts connections from

type cdpMsg struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type cdpConn struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	next    int
	pending map[int]chan cdpMsg
	closed  chan struct{}
	onEvent func(m cdpMsg) // called on the reader goroutine; keep it quick
}

func dialCDP(wsURL string, onEvent func(cdpMsg)) (*cdpConn, error) {
	cfg, err := websocket.NewConfig(wsURL, devtoolsOrigin)
	if err != nil {
		return nil, err
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}
	ws.MaxPayloadBytes = 64 << 20 // a page's HTML comes back in one message
	c := &cdpConn{ws: ws, pending: map[int]chan cdpMsg{}, closed: make(chan struct{}), onEvent: onEvent}
	go c.read()
	return c, nil
}

func (c *cdpConn) read() {
	defer close(c.closed)
	for {
		var m cdpMsg
		if err := websocket.JSON.Receive(c.ws, &m); err != nil {
			return
		}
		if m.ID == 0 {
			if c.onEvent != nil {
				c.onEvent(m)
			}
			continue
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// call sends a command (to a page when session is set) and decodes its result into out.
func (c *cdpConn) call(ctx context.Context, session, method string, params, out any) error {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan cdpMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	if err := websocket.JSON.Send(c.ws, msg); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.closed:
		return errors.New("the browser closed the connection")
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// headless is a running browser with a throwaway profile.
type headless struct {
	cmd     *exec.Cmd
	kill    context.CancelFunc // ends the browser and its helper processes
	profile string
	cdp     *cdpConn
	once    sync.Once
}

// startHeadless launches the browser at path and connects to it. Events from
// every page go to onEvent.
func startHeadless(ctx context.Context, path string, onEvent func(cdpMsg)) (*headless, error) {
	profile, err := os.MkdirTemp("", tools.BrowserProfilePrefix)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--headless", "--user-data-dir=" + profile,
		"--remote-debugging-port=0", "--remote-allow-origins=" + devtoolsOrigin,
		"--no-first-run", "--no-default-browser-check", "--disable-extensions", "--disable-sync",
		"--disable-background-networking", "--disable-component-update", "--mute-audio", "--hide-scrollbars",
		"--disable-blink-features=AutomationControlled", // some sites refuse pages that say they're automated
		"--password-store=basic", "--use-mock-keychain", // don't ask for the macOS keychain
		"--window-size=1366,900",
		"about:blank",
	}
	life, kill := context.WithCancel(context.Background()) // close() ends it, gracefully first
	cmd := tools.Command(life, path, args...)
	stderr, err := cmd.StderrPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		kill()
		os.RemoveAll(profile)
		return nil, fmt.Errorf("starting the browser: %w", err)
	}
	h := &headless{cmd: cmd, kill: kill, profile: profile}

	// It prints "DevTools listening on ws://127.0.0.1:PORT/devtools/browser/ID".
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if _, ws, ok := strings.Cut(sc.Text(), "DevTools listening on "); ok {
				found <- strings.TrimSpace(ws)
				break
			}
		}
		close(found)
		_, _ = io.Copy(io.Discard, stderr)
	}()
	var wsURL string
	select {
	case wsURL = <-found:
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
	}
	if wsURL == "" {
		h.close()
		return nil, firstErr(ctx.Err(), errors.New("the browser didn't start"))
	}
	if h.cdp, err = dialCDP(wsURL, onEvent); err != nil {
		h.close()
		return nil, fmt.Errorf("connecting to the browser: %w", err)
	}
	return h, nil
}

// close shuts the browser down (killing it if it won't go) and deletes its
// profile. It's safe to call more than once.
func (h *headless) close() { h.once.Do(h.shutdown) }

func (h *headless) shutdown() {
	if h.cdp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = h.cdp.call(ctx, "", "Browser.close", nil, nil)
		cancel()
		h.cdp.ws.Close()
	}
	done := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	h.kill() // also takes any helper processes still around
	<-done
	for range 5 { // its helper processes may still be letting go of files
		if os.RemoveAll(h.profile) == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// exited reports whether the browser has gone (crashed, or been killed).
func (h *headless) exited() bool {
	select {
	case <-h.cdp.closed:
		return true
	default:
		return false
	}
}

// newContext creates a browser context: a private window's worth of
// cookies and storage, gone when it's disposed.
func (h *headless) newContext(ctx context.Context) (string, error) {
	var r struct {
		BrowserContextID string `json:"browserContextId"`
	}
	err := h.cdp.call(ctx, "", "Target.createBrowserContext", map[string]any{"disposeOnDetach": true}, &r)
	return r.BrowserContextID, err
}

// newPage opens a tab in a browser context and returns its session id.
func (h *headless) newPage(ctx context.Context, browserContext string) (string, error) {
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := h.cdp.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": browserContext}, &target); err != nil {
		return "", err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	err := h.cdp.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	return attached.SessionID, err
}

// eval runs a JavaScript expression in the page and decodes its value into out.
func (h *headless) eval(ctx context.Context, session, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := h.cdp.call(ctx, session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, &r); err != nil {
		return err
	}
	if e := r.ExceptionDetails; e != nil {
		return fmt.Errorf("page script: %s", firstNonEmpty(e.Exception.Description, e.Text))
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}
