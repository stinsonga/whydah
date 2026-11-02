package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"whydah/internal/config"
	"whydah/internal/events"
	"whydah/internal/tools"
)

// End-to-end tests: pages served here, looked at by the crow's nest in a
// real browser and downloaded with the real helpers. They're slow and use
// the helpers Whydah has already downloaded, so they only run with
// WHYDAH_E2E set (make e2e). Their settings and app folder are throwaway.

type e2e struct {
	t     *testing.T
	ctx   context.Context
	e     *Engine
	paths tools.Paths
	media string // fixtures: clip.m3u8 with its segment, and episode.mp3
}

func newE2E(t *testing.T) *e2e {
	if os.Getenv("WHYDAH_E2E") == "" {
		t.Skip("set WHYDAH_E2E=1 to run (make e2e)")
	}
	cache, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	ytdlp := "yt-dlp"
	if runtime.GOOS == "windows" {
		ytdlp += ".exe"
	}
	if _, err := os.Stat(filepath.Join(cache, "bin", ytdlp)); err != nil {
		t.Skip("no helpers yet: run Whydah once to download them")
	}
	home := t.TempDir() // settings, the queue and the sign-in profile go here, not in the real app folder
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)

	settings, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	settings.Override(func(s *config.Settings) { s.OutputDir, s.BrowserCookies = out, "" })
	hub := events.NewHub()
	tm := tools.New(cache, hub, func() string { return "stable" })
	if !tm.BrowserReady() {
		t.Skip("no Chromium-family browser installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	tm.Start(ctx)
	paths, err := tm.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if paths.FFmpegDir == "" {
		t.Skip("no ffmpeg")
	}
	e := New(settings, tm, hub)
	t.Cleanup(func() { e.Shutdown(5 * time.Second) })
	x := &e2e{t: t, ctx: ctx, e: e, paths: paths, media: t.TempDir()}
	x.makeMedia()
	return x
}

// makeMedia makes an 8-second HLS clip and an 8-second MP3: streams shorter
// than minStreamDuration are taken for ads and skipped.
func (x *e2e) makeMedia() {
	ffmpeg := filepath.Join(x.paths.FFmpegDir, "ffmpeg")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc=size=160x90:rate=15:duration=8", "-f", "lavfi", "-i", "sine=duration=8",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
			"-f", "hls", "-hls_time", "8", "-hls_list_size", "0", "-hls_segment_filename", filepath.Join(x.media, "clip_%d.ts"),
			filepath.Join(x.media, "clip.m3u8")},
		{"-f", "lavfi", "-i", "sine=frequency=440:duration=8", "-c:a", "libmp3lame", "-b:a", "32k", filepath.Join(x.media, "episode.mp3")},
	} {
		if out, err := exec.Command(ffmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			x.t.Fatalf("making test media: %v\n%s", err, out)
		}
	}
}

// serveMedia serves the fixture named by the last part of the request's path.
func (x *e2e) serveMedia(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, filepath.Join(x.media, filepath.Base(r.URL.Path)))
}

// server serves h on two origins, http://127.0.0.1:port and
// http://localhost:port, for pages that embed a player from another site.
func (x *e2e) server(h http.HandlerFunc) (site, other string) {
	srv := httptest.NewServer(h)
	x.t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv.URL, "http://localhost:" + u.Port()
}

// inspect looks at page, and fails the test if it can't.
func (x *e2e) inspect(page string) Metadata {
	md, err := x.e.Inspect(x.ctx, page, InspectOptions{})
	if err != nil {
		x.t.Fatalf("inspecting %s: %v", page, err)
	}
	return md
}

// stream finds the stream whose link contains part.
func (x *e2e) stream(md Metadata, part string) Stream {
	for _, st := range md.Streams {
		if strings.Contains(st.URL, part) {
			return st
		}
	}
	x.t.Fatalf("no stream with %q among %+v (kind %q)", part, md.Streams, md.Kind)
	return Stream{}
}

// finish waits for jobs to finish, and fails the test unless each is done
// with its file saved.
func (x *e2e) finish(jobs ...Job) []Job {
	for i, j := range jobs {
		for {
			cur, ok := x.e.Job(j.ID)
			if !ok {
				x.t.Fatalf("job %s went missing", j.ID)
			}
			if cur.State.Finished() {
				if cur.State != Done {
					x.t.Fatalf("job %q: %s: %s", cur.Title, cur.State, cur.Error)
				}
				if _, err := os.Stat(cur.FilePath); err != nil {
					x.t.Fatalf("job %q is done but its file isn't there: %v", cur.Title, err)
				}
				jobs[i] = cur
				break
			}
			select {
			case <-x.ctx.Done():
				x.t.Fatalf("job %q didn't finish: %s", cur.Title, cur.State)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return jobs
}

// A player in an iframe from another site, whose stream is only served to
// that site: the download must send the player's Referer, not the page's.
func TestE2EIframeStream(t *testing.T) {
	x := newE2E(t)
	var site, player string
	site, player = x.server(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			fmt.Fprintf(w, `<!doctype html><title>Embed page</title><iframe src="%s/player" width=640 height=360></iframe>`, player)
		case r.URL.Path == "/player":
			fmt.Fprintf(w, `<!doctype html><title>Player</title><video muted></video>
<script>var p="%s/hls/"+"clip.m3u8";fetch(p);document.querySelector("video").src=p;</script>`, site)
		case strings.HasPrefix(r.URL.Path, "/hls/"):
			if !strings.HasPrefix(r.Referer(), player+"/") {
				http.Error(w, "hotlinking", http.StatusForbidden)
				return
			}
			x.serveMedia(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	md := x.inspect(site + "/")
	st := x.stream(md, "/hls/clip.m3u8")
	if !strings.HasPrefix(st.Referer, player+"/") {
		t.Errorf("stream's Referer = %q, want the player's, %s/…", st.Referer, player)
	}
	x.finish(x.e.Download(Request{URL: st.URL, Title: st.Title, Referer: st.Referer, Origin: st.Origin, Page: site + "/"}))
}

// A podcast page whose audio player gets its file from a script.
func TestE2EScriptAudio(t *testing.T) {
	x := newE2E(t)
	site, _ := x.server(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/episode":
			fmt.Fprint(w, `<!doctype html><title>Episode 1</title><audio controls></audio>
<script>var a=document.querySelector("audio");a.src="/files/"+"episode.mp3";a.load();fetch(a.src);</script>`)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			x.serveMedia(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	md := x.inspect(site + "/episode")
	st := x.stream(md, "/files/episode.mp3")
	if st.Kind != "audio" {
		t.Errorf("stream kind = %q, want audio", st.Kind)
	}
	x.finish(x.e.Download(Request{URL: st.URL, Audio: true, Title: st.Title, Referer: st.Referer, Origin: st.Origin, Page: site + "/episode"}))
}

// A listing of videos whose pages have a player yt-dlp can't read: checking
// the links falls back to the crow's nest, which catches each page's stream.
func TestE2ELinkedPlayers(t *testing.T) {
	x := newE2E(t)
	site, _ := x.server(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, `<!doctype html><title>Listing</title><main>
<a href="/watch/1"><img src="/thumb.png" alt="Clip 1" width=320 height=180><span class="duration">0:08</span> Clip 1</a>
<a href="/watch/2"><img src="/thumb.png" alt="Clip 2" width=320 height=180><span class="duration">0:08</span> Clip 2</a>
</main>`)
		case strings.HasPrefix(r.URL.Path, "/watch/"):
			n := filepath.Base(r.URL.Path)
			fmt.Fprintf(w, `<!doctype html><title>Clip %s</title><video muted></video>
<script>var p="/hls/%s/"+"clip.m3u8";fetch(p);document.querySelector("video").src=p;</script>`, n, n)
		case r.URL.Path == "/thumb.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(onePixelPNG)
		case strings.HasPrefix(r.URL.Path, "/hls/"):
			x.serveMedia(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	md := x.inspect(site + "/")
	if len(md.Links) != 2 {
		t.Fatalf("found %d linked videos, want 2: %+v (kind %q)", len(md.Links), md.Links, md.Kind)
	}
	entries, err := x.e.CheckLinks(x.ctx, site+"/", md.Links)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("checking the links found %d videos, want 2: %+v", len(entries), entries)
	}
	var items []Item
	for i, en := range entries {
		want := fmt.Sprintf("/hls/%d/clip.m3u8", i+1)
		if !strings.Contains(en.URL, want) || en.Page != site+fmt.Sprintf("/watch/%d", i+1) {
			t.Errorf("entry %d: %s from %q, want the stream %s from its page", i+1, en.URL, en.Page, want)
		}
		items = append(items, Item{Index: en.Index, URL: en.URL, Title: en.Title, StreamFrom: en.StreamFrom})
	}
	x.finish(x.e.DownloadPlaylist(md.Title, len(items), false, items)...)
}

// A stream link that expired while it waited: the download gets a fresh
// one from the page it came from.
func TestE2EStaleStream(t *testing.T) {
	x := newE2E(t)
	var mu sync.Mutex
	token, loads := 100, 0
	site, _ := x.server(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/watch":
			token++
			loads++
			fmt.Fprintf(w, `<!doctype html><title>Fresh clip</title><video muted></video><script>fetch("/hls/clip.m3u8?token=%d")</script>`, token)
		case r.URL.Path == "/hls/clip.m3u8" && r.URL.Query().Get("token") != fmt.Sprint(token):
			http.Error(w, "expired", http.StatusForbidden)
		case strings.HasPrefix(r.URL.Path, "/hls/"):
			x.serveMedia(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	j := x.finish(x.e.Download(Request{URL: site + "/hls/clip.m3u8?token=7", Title: "Fresh clip", Referer: site + "/", Page: site + "/watch"}))[0]
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(j.URL, "token=101") || loads != 1 {
		t.Errorf("finished with %s after %d page loads; want token=101 after 1", j.URL, loads)
	}
}

// onePixelPNG is a transparent 1×1 PNG.
var onePixelPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")
