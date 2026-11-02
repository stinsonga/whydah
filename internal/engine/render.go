package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/url"
	"strings"
	"sync"
	"time"

	"whydah/internal/config"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

// The crow's nest: load the page in a hidden browser, let its scripts run,
// scroll to the bottom so lazy-loaded and infinite-scroll images appear, then
// collect what's on the page (and any image the browser downloaded that the
// page no longer shows, such as earlier carousel slides).

const (
	renderTimeout = 90 * time.Second // the whole look, including starting the browser
	streamTimeout = 45 * time.Second // a streams-only look at a linked page
	scrollBudget  = 12 * time.Second // endless feeds stop here
	maxScrolls    = 30
)

// collectJS lists the page's images in order, with what the browser knows
// about them: the size it actually loaded, the srcset it chose from, and the
// link around it. Then the rendered HTML, for the same checks as a page scan.
const collectJS = `(() => {
  const isImg = u => { try { return /\.(jpe?g|png|gif|webp|avif|heic|bmp|tiff?|jfif)$/i.test(new URL(u, location.href).pathname) } catch (e) { return false } };
  const items = [];
  const all = document.querySelectorAll('*');
  for (let i = 0; i < all.length && i < 30000; i++) {
    const el = all[i];
    if (el.tagName === 'IMG') {
      let srcset = el.srcset || '';
      const pic = el.parentElement && el.parentElement.tagName === 'PICTURE' ? el.parentElement : null;
      if (pic) for (const s of pic.querySelectorAll('source')) if (s.srcset) srcset += (srcset ? ', ' : '') + s.srcset;
      const a = el.closest('a[href]');
      items.push({kind: 'img', src: el.currentSrc || el.src, srcset, w: el.naturalWidth, h: el.naturalHeight, href: a && isImg(a.href) ? a.href : ''});
    } else if (el.tagName === 'A' && isImg(el.href) && !el.querySelector('img')) {
      items.push({kind: 'link', src: el.href});
    } else {
      const bg = getComputedStyle(el).backgroundImage;
      if (bg && bg !== 'none') {
        const r = el.getBoundingClientRect();
        if (r.width >= 160 || r.height >= 160)
          for (const m of bg.matchAll(/url\(["']?(.*?)["']?\)/g)) items.push({kind: 'bg', src: new URL(m[1], location.href).href});
      }
    }
  }
  const og = document.querySelector('meta[property="og:title"]');
  return {url: location.href, title: (og && og.content) || document.title, items, html: document.documentElement.outerHTML.slice(0, 10000000)};
})()`

const scrollJS = `(() => { scrollBy(0, innerHeight * 0.85); return {y: scrollY, height: document.documentElement.scrollHeight, view: innerHeight}; })()`

type renderedItem struct {
	Kind   string `json:"kind"` // "img", "link" or "bg"
	Src    string `json:"src"`
	Srcset string `json:"srcset"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Href   string `json:"href"`
}

type renderedPage struct {
	URL   string         `json:"url"`
	Title string         `json:"title"`
	Items []renderedItem `json:"items"`
	HTML  string         `json:"html"`
}

// pageWatch follows one page's network activity (and that of its
// cross-site iframes, which are separate targets), to know when it has
// settled, which images it downloaded and which streams it played.
type pageWatch struct {
	session  string
	attached chan struct{} // signalled when attaching grows

	mu        sync.Mutex
	frames    map[string]bool      // iframe sessions
	attaching []attachedTarget     // targets Chrome is holding until they're set up
	inflight  map[string]time.Time // request id -> when it started
	sent      map[string]sentRequest
	last      time.Time // last request started or finished
	loaded    bool
	images    []string // image responses, in arrival order
	streams   []Stream // in arrival order
	seen      map[string]bool
}

// maxFrames is how many of a page's iframes are watched; a page with more
// is mostly ads, and the rest go unwatched.
const maxFrames = 64

// attachedTarget is an iframe, worker or the like that Chrome started for
// the page, paused until it's set up (frame: watched) and resumed.
type attachedTarget struct {
	session string
	frame   bool
}

// sentRequest is what a stream needs of its request: who it said it came from.
type sentRequest struct{ referer, origin, document string }

func newPageWatch() *pageWatch {
	return &pageWatch{attached: make(chan struct{}, 1), frames: map[string]bool{}, inflight: map[string]time.Time{},
		sent: map[string]sentRequest{}, seen: map[string]bool{}, last: time.Now()}
}

// sessions lists the page's session and its iframes'.
func (w *pageWatch) sessions() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []string{w.session}
	for s := range w.frames {
		out = append(out, s)
	}
	return out
}

func (w *pageWatch) event(m cdpMsg) {
	w.mu.Lock()
	ours := m.SessionID == w.session || w.frames[m.SessionID]
	w.mu.Unlock()
	if !ours {
		return
	}
	var p struct {
		RequestID   string `json:"requestId"`
		Type        string `json:"type"`
		DocumentURL string `json:"documentURL"`
		Request     struct {
			Headers map[string]string `json:"headers"`
		} `json:"request"`
		Response struct {
			URL      string `json:"url"`
			Status   int    `json:"status"`
			MimeType string `json:"mimeType"`
		} `json:"response"`
		SessionID  string `json:"sessionId"` // Target.attachedToTarget
		TargetInfo struct {
			Type string `json:"type"`
		} `json:"targetInfo"`
	}
	switch m.Method {
	case "Network.requestWillBeSent", "Network.loadingFinished", "Network.loadingFailed", "Network.responseReceived", "Page.loadEventFired", "Target.attachedToTarget":
		_ = json.Unmarshal(m.Params, &p)
	default:
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = time.Now()
	switch m.Method {
	case "Target.attachedToTarget":
		// Every target attached is paused (waitForDebuggerOnStart), so
		// an iframe's first requests aren't missed while it's set up, and
		// must be resumed, watched or not.
		frame := p.TargetInfo.Type == "iframe" && !w.frames[p.SessionID] && len(w.frames) < maxFrames
		if frame {
			w.frames[p.SessionID] = true
		}
		w.attaching = append(w.attaching, attachedTarget{p.SessionID, frame})
		select {
		case w.attached <- struct{}{}:
		default:
		}
	case "Network.requestWillBeSent":
		w.inflight[p.RequestID] = time.Now()
		h := func(name string) string {
			for k, v := range p.Request.Headers {
				if strings.EqualFold(k, name) {
					return v
				}
			}
			return ""
		}
		w.sent[p.RequestID] = sentRequest{h("Referer"), h("Origin"), p.DocumentURL}
	case "Network.loadingFinished", "Network.loadingFailed":
		delete(w.inflight, p.RequestID)
	case "Page.loadEventFired":
		if m.SessionID == w.session {
			w.loaded = true
		}
	case "Network.responseReceived":
		u, status := p.Response.URL, p.Response.Status
		ctype, _, _ := mime.ParseMediaType(p.Response.MimeType)
		if p.Type == "Image" && status == 200 && strings.HasPrefix(ctype, "image/") && ctype != "image/svg+xml" &&
			strings.HasPrefix(u, "http") && !w.seen[u] && m.SessionID == w.session {
			w.seen[u] = true
			w.images = append(w.images, u)
		}
		if kind := streamKind(u, p.Response.MimeType, p.Type); kind != "" && (status == 200 || status == 206) && !w.seen[u] {
			w.seen[u] = true
			req := w.sent[p.RequestID]
			w.streams = append(w.streams, Stream{URL: u, Kind: kind, Referer: firstNonEmpty(req.referer, req.document), Origin: req.origin})
		}
	}
}

// settle waits until the page has been quiet for quiet, or until limit:
// at most two requests open, not counting ones open for 5s, which are
// long polls and streams rather than content. Busy pages hit the limit.
func (w *pageWatch) settle(ctx context.Context, quiet, limit time.Duration) {
	deadline := time.Now().Add(limit)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(deadline) {
		w.mu.Lock()
		open := 0
		for _, started := range w.inflight {
			if time.Since(started) < 5*time.Second {
				open++
			}
		}
		idle := open <= 2 && time.Since(w.last) >= quiet
		w.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *pageWatch) waitLoaded(ctx context.Context, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		w.mu.Lock()
		loaded := w.loaded
		w.mu.Unlock()
		if loaded {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// renderPhotos is the crow's nest for pageURL.
func (e *Engine) renderPhotos(ctx context.Context, paths tools.Paths, s config.Settings, pageURL string) (Metadata, error) {
	return e.render(ctx, paths, s, pageURL, false)
}

// renderStreams is a shorter look at pageURL for its streams alone, for a
// linked page yt-dlp can't read: no scrolling, and no photos.
func (e *Engine) renderStreams(ctx context.Context, paths tools.Paths, s config.Settings, pageURL string) (Metadata, error) {
	return e.render(ctx, paths, s, pageURL, true)
}

func (e *Engine) render(ctx context.Context, paths tools.Paths, s config.Settings, pageURL string, streamsOnly bool) (Metadata, error) {
	if e.usesSignIn(s) { // while the sign-in window is open, and not against the time limit
		if err := e.browsers.waitSignIn(ctx); err != nil {
			return Metadata{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, map[bool]time.Duration{false: renderTimeout, true: streamTimeout}[streamsOnly])
	defer cancel()
	var browser, profile string
	var err error
	if e.usesSignIn(s) { // the page sees what the sign-in window saw
		browser, profile = e.tools.VisibleBrowser(), signInProfile()
	} else if browser, err = e.tools.Browser(ctx); err != nil {
		return Metadata{}, err
	}
	var jar cookieJar
	if profile == "" {
		if jar, err = e.siteJar(ctx, paths, s, pageURL); err != nil {
			slog.Warn("couldn't read browser cookies", "browser", s.BrowserCookies, "err", err)
		}
	}
	release := e.tools.Use() // a refit mustn't replace a headless Chrome that's running
	defer release()

	start := time.Now()
	w := newPageWatch()
	h, session, closeTab, err := e.browsers.tab(ctx, browser, profile, w) // events before the page's session is known are ignored
	if err != nil {
		if ctx.Err() != nil {
			return Metadata{}, ctx.Err()
		}
		if ie, ok := err.(*i18n.Error); ok {
			return Metadata{}, ie
		}
		return Metadata{}, i18n.New("error.browser_start").WithDetail(err.Error())
	}
	defer closeTab()
	w.mu.Lock()
	w.session = session
	w.mu.Unlock()

	var version struct {
		UserAgent string `json:"userAgent"`
	}
	_ = h.cdp.call(ctx, "", "Browser.getVersion", nil, &version)
	userAgent := map[string]any{"userAgent": strings.ReplaceAll(version.UserAgent, "HeadlessChrome", "Chrome")} // sites turn away "HeadlessChrome"
	autoAttach := map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}
	for _, c := range []struct {
		method string
		params any
	}{
		{"Network.enable", nil},
		{"Page.enable", nil},
		// A large, high-density screen, so srcset picks big images.
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": 1366, "height": 900, "deviceScaleFactor": 2, "mobile": false}},
		{"Network.setUserAgentOverride", userAgent},
		// Cross-site iframes (embedded players, mostly) are targets of their own.
		{"Target.setAutoAttach", autoAttach},
	} {
		if err := h.cdp.call(ctx, session, c.method, c.params, nil); err != nil {
			return Metadata{}, err
		}
	}
	go func() { // set up each iframe as it attaches, so its streams are seen too
		for {
			select {
			case <-w.attached:
				w.mu.Lock()
				targets := w.attaching
				w.attaching = nil
				w.mu.Unlock()
				for _, t := range targets {
					if t.frame {
						_ = h.cdp.call(ctx, t.session, "Network.setUserAgentOverride", userAgent, nil)
						_ = h.cdp.call(ctx, t.session, "Network.enable", nil, nil)
						_ = h.cdp.call(ctx, t.session, "Target.setAutoAttach", autoAttach, nil)
					}
					_ = h.cdp.call(ctx, t.session, "Runtime.runIfWaitingForDebugger", nil, nil)
				}
			case <-ctx.Done():
				return
			case <-h.cdp.closed:
				return
			}
		}
	}()
	if len(jar) > 0 {
		if err := h.cdp.call(ctx, session, "Network.setCookies", map[string]any{"cookies": jar.cdp()}, nil); err != nil {
			slog.Warn("couldn't set cookies in the browser", "err", err)
		}
	}

	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := h.cdp.call(ctx, session, "Page.navigate", map[string]any{"url": pageURL}, &nav); err != nil {
		return Metadata{}, err
	}
	if nav.ErrorText != "" {
		return Metadata{}, i18n.New("error.page_open").WithDetail(nav.ErrorText)
	}
	w.waitLoaded(ctx, 25*time.Second)
	loadedAt := time.Since(start)
	w.settle(ctx, time.Second, 4*time.Second)
	settledAt := time.Since(start)

	// Start the players, in the page and its iframes, so they load their streams.
	played := 0
	for _, sess := range w.sessions() {
		var n int
		if h.eval(ctx, sess, playJS, &n) == nil {
			played += n
		}
	}
	if played > 0 {
		w.settle(ctx, time.Second, 6*time.Second)
	}

	scrollStart, atBottom, failures, scrolls := time.Now(), 0, 0, 0
	stop := "budget"
	if streamsOnly {
		stop = "streams only"
	}
	for ; !streamsOnly && scrolls < maxScrolls && time.Since(scrollStart) < scrollBudget && ctx.Err() == nil; scrolls++ {
		var pos struct{ Y, Height, View float64 }
		if err := h.eval(ctx, session, scrollJS, &pos); err != nil {
			// e.g. the page replaced itself (a redirect or a bot check passing)
			if failures++; failures >= 3 {
				stop = err.Error()
				break
			}
			w.settle(ctx, 600*time.Millisecond, 2500*time.Millisecond)
			continue
		}
		w.settle(ctx, 600*time.Millisecond, 2500*time.Millisecond)
		if pos.Y+pos.View >= pos.Height-2 {
			if atBottom++; atBottom >= 2 { // and nothing more loaded since
				stop = "bottom"
				break
			}
		} else {
			atBottom = 0
		}
	}

	scrolledAt := time.Since(start)
	var page renderedPage
	if err := h.eval(ctx, session, collectJS, &page); err != nil {
		return Metadata{}, err
	}
	var media []mediaElement
	for _, sess := range w.sessions() {
		var m []mediaElement
		if h.eval(ctx, sess, mediaJS, &m) == nil {
			media = append(media, m...)
		}
	}
	w.mu.Lock()
	network := append([]string(nil), w.images...)
	streams := streamCands(pageURL, w.streams, media)
	frames := len(w.frames)
	w.mu.Unlock()
	closeTab() // done with the browser; probing doesn't need it
	// The photos are fetched with the cookies the page had.
	if profile != "" && !streamsOnly {
		if jar, err = e.siteJar(ctx, paths, s, pageURL); err != nil {
			slog.Warn("couldn't read the sign-in browser's cookies", "err", err)
		}
	}

	base, err := url.Parse(page.URL)
	if err != nil || base.Scheme == "" {
		base, _ = url.Parse(pageURL)
	}
	scan := extractPage(base, strings.NewReader(page.HTML))
	cands := renderedCandidates(base, page, scan.photos, network)
	md := Metadata{URL: pageURL, Kind: "photos", Source: "browser", Title: firstNonEmpty(strings.TrimSpace(page.Title), base.Hostname())}
	md.Links = videoLinks(base, scan.links)
	checked := make(chan []Stream, 1)
	go func() { // yt-dlp checks the streams while the photos are measured
		var poster string
		for _, m := range media {
			poster = firstNonEmpty(poster, m.Poster)
		}
		thumb := firstNonEmpty(poster, scan.og)
		checked <- e.checkStreams(ctx, paths, s, md.Title, thumb, streams)
	}()
	if !streamsOnly {
		md.Photos = probePhotos(ctx, pageURL, jar, cands)
	}
	md.Streams = <-checked
	if len(md.Photos) > 0 {
		md.Thumbnail = firstNonEmpty(md.Photos[0].Thumb, md.Photos[0].URL)
	} else if len(md.Streams) > 0 {
		md.Thumbnail = md.Streams[0].Thumbnail
	}
	slog.Info("crow's nest", "url", pageURL, "browser", browser, "signed in", profile != "", "candidates", len(cands), "photos", len(md.Photos),
		"links", len(md.Links), "streams seen", len(streams), "streams", len(md.Streams), "players", played, "iframes", frames,
		"cookies", len(jar), "loaded", loadedAt.Round(100*time.Millisecond), "settled", settledAt.Round(100*time.Millisecond),
		"scrolled", scrolledAt.Round(100*time.Millisecond), "scrolls", scrolls, "stop", stop, "took", time.Since(start).Round(100*time.Millisecond))
	if ctx.Err() != nil && len(md.Photos) == 0 && len(md.Streams) == 0 {
		return Metadata{}, i18n.New("error.page_timeout")
	}
	return md, nil
}

// renderedCandidates puts what the browser showed first (with real sizes
// where it loaded them), then what a page scan found in the rendered HTML,
// then images it downloaded that the page doesn't show any more.
func renderedCandidates(base *url.URL, page renderedPage, static []Photo, network []string) []Photo {
	var cands []Photo
	seen := map[string]bool{}
	add := func(p Photo) {
		if p.URL == "" || seen[p.URL] || len(cands) >= maxCandidates {
			return
		}
		seen[p.URL] = true
		if p.Thumb == p.URL {
			p.Thumb = ""
		}
		cands = append(cands, p)
	}
	// Every size of an image the browser showed counts as that image, so the
	// page scan below doesn't add its srcset sizes again as other photos.
	shown := func(it renderedItem) {
		for _, ref := range append(srcsetURLs(it.Srcset), it.Src, it.Href) {
			if u := resolve(base, ref); u != nil {
				seen[u.String()] = true
			}
		}
	}
	for _, it := range page.Items {
		src := resolve(base, it.Src)
		full := src
		if large := resolve(base, largestSrc(it.Srcset)); large != nil {
			full = large
		}
		if href := resolve(base, it.Href); href != nil {
			full = href // the link is the full size; what's shown is its thumbnail
		}
		if full == nil || skippable(full) {
			continue
		}
		p := Photo{URL: full.String()}
		switch {
		case src != nil && full.String() == src.String() && it.W > 0:
			p.Width, p.Height, p.measured = it.W, it.H, true
		case src != nil:
			p.Thumb = src.String()
		}
		add(p)
		shown(it)
	}
	for _, p := range static {
		add(p)
	}
	for _, link := range network {
		if u := resolve(base, link); u != nil && !skippable(u) {
			add(Photo{URL: u.String()})
		}
	}
	return cands
}

// srcsetURLs lists every candidate in a srcset.
func srcsetURLs(srcset string) []string {
	var out []string
	for _, part := range strings.Split(srcset, ",") {
		if f := strings.Fields(part); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}
