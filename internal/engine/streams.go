package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"whydah/internal/config"
	"whydah/internal/tools"
)

// Stream sniffing: while the crow's nest has a page open, the HLS and DASH
// manifests and the video and audio files its player loads are noted, so
// a site with its own player (one yt-dlp has no extractor for) can still be
// salvaged. Each is checked with yt-dlp, sending the Referer the player
// sent, since stream servers often refuse requests without it.

// Stream is a video or audio stream the page played.
type Stream struct {
	URL            string  `json:"url"`
	Kind           string  `json:"kind"` // "hls", "dash", "video" or "audio"
	Referer        string  `json:"referer"`
	Origin         string  `json:"origin,omitempty"`
	Title          string  `json:"title"`
	Duration       float64 `json:"duration"`
	DurationString string  `json:"duration_string"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	Filesize       int64   `json:"filesize,omitempty"`
	Live           bool    `json:"live,omitempty"`
	Thumbnail      string  `json:"thumbnail,omitempty"`
}

const (
	maxStreams        = 8               // checked with yt-dlp; more are almost always ads or previews
	minStreamDuration = 5 * time.Second // shorter ones are bumpers, ads and hover previews
)

// streamKind says which kind of stream a response is, or "" if it isn't
// one. Manifests count however they were fetched (players load them with
// XHR); plain files only when a <video> or <audio> loaded them, since a
// player that fetches media itself is fetching segments.
func streamKind(rawURL, mimeType, resourceType string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || adHost(u.Hostname()) {
		return ""
	}
	ctype, _, _ := mime.ParseMediaType(mimeType)
	ext := strings.ToLower(path.Ext(u.Path))
	switch {
	case ext == ".m3u8" || ctype == "application/vnd.apple.mpegurl" || ctype == "application/x-mpegurl" || ctype == "audio/mpegurl" || ctype == "audio/x-mpegurl":
		return "hls"
	case ext == ".mpd" || ctype == "application/dash+xml":
		return "dash"
	case resourceType != "Media" || segmentExt[ext] || ctype == "video/mp2t" || ctype == "video/iso.segment":
		return ""
	case strings.HasPrefix(ctype, "video/") || mediaExt[ext] == "video":
		return "video"
	case strings.HasPrefix(ctype, "audio/") || mediaExt[ext] == "audio":
		return "audio"
	}
	return ""
}

var segmentExt = map[string]bool{".ts": true, ".m4s": true, ".m4f": true, ".m4v_seg": true, ".cmfv": true, ".cmfa": true, ".aac": true}

var mediaExt = map[string]string{
	".mp4": "video", ".m4v": "video", ".webm": "video", ".mov": "video", ".mkv": "video", ".ogv": "video",
	".mp3": "audio", ".m4a": "audio", ".ogg": "audio", ".oga": "audio", ".opus": "audio", ".wav": "audio", ".flac": "audio",
}

// adHost reports whether host serves ads, whose clips play before the real video.
func adHost(host string) bool {
	host = strings.ToLower(host)
	for _, ad := range []string{"doubleclick.net", "googlesyndication.com", "imasdk.googleapis.com", "googleadservices.com",
		"adnxs.com", "springserve.com", "spotxchange.com", "spotx.tv", "moatads.com", "adsrvr.org", "serving-sys.com",
		"innovid.com", "teads.tv", "tremorhub.com", "aniview.com", "connatix.com", "vidazoo.com", "advertising.com", "amazon-adsystem.com"} {
		if host == ad || strings.HasSuffix(host, "."+ad) {
			return true
		}
	}
	return false
}

// playJS starts the page's players: every <video> and <audio> plays muted
// (autoplay rules allow that), and a large play button is clicked, since
// many players load nothing until then. Buttons inside links are left
// alone: those open another page. Returns how many it started.
const playJS = `(() => {
  let n = 0;
  for (const m of document.querySelectorAll('video, audio')) {
    m.muted = true;
    try { const p = m.play(); if (p && p.catch) p.catch(() => {}); n++ } catch (e) {}
  }
  const sel = '.vjs-big-play-button, .jw-display-icon-display, .jw-icon-display, .plyr__control--overlaid, .mejs__overlay-button, ' +
    '.fp-play, .fp-ui, .vp-big-play-button, [class*="play-button" i], [class*="playbutton" i], [class*="play-btn" i], ' +
    '[class*="bigplay" i], button[aria-label], [role="button"][aria-label], [data-testid*="play" i]';
  let clicked = 0;
  for (const b of document.querySelectorAll(sel)) {
    const label = b.getAttribute('aria-label');
    if (label !== null && !/\bplay\b/i.test(label)) continue;
    if (b.closest('a[href]')) continue;
    const r = b.getBoundingClientRect();
    if (r.width < 24 || r.height < 24 || r.bottom < 0 || r.top > innerHeight * 2) continue;
    try { b.click(); n++ } catch (e) {}
    if (++clicked >= 2) break;
  }
  return n;
})()`

// mediaJS lists the media elements' sources that are plain links (not the
// blob: a script-fed player shows), and their posters, for players that
// load nothing until they're played.
const mediaJS = `(() => [...document.querySelectorAll('video, audio')].map(m => ({
  tag: m.tagName.toLowerCase(),
  srcs: [m.currentSrc, m.src, ...[...m.querySelectorAll('source')].map(s => s.src)].filter(s => /^https?:/.test(s || '')),
  poster: m.poster || ''
})))()`

type mediaElement struct {
	Tag    string   `json:"tag"`
	Srcs   []string `json:"srcs"`
	Poster string   `json:"poster"`
}

// streamCands adds the media elements' own sources to what the network
// showed, keeping network order (the first stream is usually the main video).
func streamCands(pageURL string, network []Stream, media []mediaElement) []Stream {
	out := append([]Stream(nil), network...)
	seen := map[string]bool{}
	for _, s := range out {
		seen[s.URL] = true
	}
	for _, m := range media {
		for _, src := range m.Srcs {
			if seen[src] {
				continue
			}
			kind := streamKind(src, "", "Media") // the element would load it as media
			if kind == "" && m.Tag == "audio" && strings.HasPrefix(src, "http") && !adHost(hostOf(src)) {
				kind = "audio" // a podcast player's link with no extension
			}
			if kind != "" {
				seen[src] = true
				out = append(out, Stream{URL: src, Kind: kind, Referer: pageURL})
			}
		}
	}
	return out
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

// checkStreams asks yt-dlp about each stream, a few at a time, and keeps the
// ones it can download, in their original order. A master playlist stands
// for the variant playlists it lists, so those are dropped.
func (e *Engine) checkStreams(ctx context.Context, paths tools.Paths, s config.Settings, title, thumb string, cands []Stream) []Stream {
	if len(cands) > maxStreams {
		cands = cands[:maxStreams]
	}
	type result struct {
		s       Stream
		ok      bool
		formats []string // the stream URLs inside it
	}
	results := make([]result, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := e.probeStream(ctx, paths, s, c)
			if err != nil {
				slog.Info("stream not downloadable", "url", c.URL, "err", err)
				return
			}
			c.Duration, c.DurationString = info.Duration, formatDuration(info.Duration)
			c.Width, c.Height, c.Live = info.Width, info.Height, info.IsLive
			c.Filesize = max(info.Filesize, info.FilesizeApprox)
			var formats []string
			for _, f := range info.Formats {
				formats = append(formats, f.URL, f.ManifestURL)
				c.Width, c.Height = max(c.Width, f.Width), max(c.Height, f.Height)
				c.Filesize = max(c.Filesize, f.Filesize, f.FilesizeApprox)
			}
			results[i] = result{s: c, ok: true, formats: formats}
		}()
	}
	wg.Wait()

	inside := map[string]bool{}
	for _, r := range results {
		for _, f := range r.formats {
			if f != r.s.URL {
				inside[f] = true
			}
		}
	}
	var out []Stream
	for _, r := range results {
		if !r.ok || inside[r.s.URL] || (!r.s.Live && r.s.Duration > 0 && r.s.Duration < minStreamDuration.Seconds()) {
			continue
		}
		out = append(out, r.s)
	}
	for i := range out {
		out[i].Title = title
		if len(out) > 1 {
			out[i].Title = title + " (" + strconv.Itoa(i+1) + ")"
		}
		out[i].Thumbnail = thumb
	}
	return out
}

type streamInfo struct {
	ytInfo
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	IsLive  bool `json:"is_live"`
	Formats []struct {
		URL            string `json:"url"`
		ManifestURL    string `json:"manifest_url"`
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		Filesize       int64  `json:"filesize"`
		FilesizeApprox int64  `json:"filesize_approx"`
	} `json:"formats"`
}

func (e *Engine) probeStream(ctx context.Context, paths tools.Paths, s config.Settings, st Stream) (streamInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cookies, done := e.cookieFile(ctx, s)
	defer done()
	args := append(commonArgs(paths, s, cookies), "-J", "--no-playlist", "--socket-timeout", "15")
	args = append(append(args, streamArgs(st.Referer, st.Origin)...), "--", st.URL)
	release := e.tools.Use()
	defer release()
	cmd := tools.Command(ctx, paths.YtDlp, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return streamInfo{}, ytError(stderr.String(), err)
	}
	var info streamInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return streamInfo{}, err
	}
	return info, nil
}

// streamArgs send a stream's request the way the page's player did.
func streamArgs(referer, origin string) []string {
	var args []string
	if referer != "" {
		args = append(args, "--referer", referer)
	}
	if origin != "" {
		args = append(args, "--add-header", "Origin:"+origin)
	}
	return args
}
