package engine

import (
	"net/url"
	"strings"
	"testing"
)

func TestStreamKind(t *testing.T) {
	for _, c := range []struct {
		url, mime, typ, want string
	}{
		{"https://cdn.example.com/v/master.m3u8", "application/vnd.apple.mpegurl", "XHR", "hls"},
		{"https://cdn.example.com/v/playlist", "application/x-mpegURL", "Fetch", "hls"},
		{"https://cdn.example.com/v/manifest.mpd", "", "XHR", "dash"},
		{"https://cdn.example.com/clip.mp4", "video/mp4", "Media", "video"},
		{"https://cdn.example.com/episode.mp3?x=1", "audio/mpeg", "Media", "audio"},
		{"https://cdn.example.com/clip.mp4", "video/mp4", "XHR", ""},     // a player fetching segments itself
		{"https://cdn.example.com/seg-12.ts", "video/mp2t", "Media", ""}, // an HLS segment
		{"https://cdn.example.com/chunk-3.m4s", "video/mp4", "Media", ""},
		{"https://imasdk.googleapis.com/ad.mp4", "video/mp4", "Media", ""}, // an ad
		{"https://ads.doubleclick.net/x/master.m3u8", "", "XHR", ""},       // an ad's manifest
		{"blob:https://example.com/1234", "video/mp4", "Media", ""},        // what MSE players show
		{"https://example.com/page.html", "text/html", "Document", ""},
	} {
		if got := streamKind(c.url, c.mime, c.typ); got != c.want {
			t.Errorf("streamKind(%s, %s, %s) = %q, want %q", c.url, c.mime, c.typ, got, c.want)
		}
	}
}

func TestStreamCands(t *testing.T) {
	network := []Stream{{URL: "https://cdn.example.com/master.m3u8", Kind: "hls", Referer: "https://player.example.net/embed/1"}}
	media := []mediaElement{
		{Tag: "video", Srcs: []string{"https://cdn.example.com/master.m3u8", "https://cdn.example.com/fallback.mp4"}, Poster: "https://example.com/p.jpg"},
		{Tag: "audio", Srcs: []string{"https://feeds.example.com/play/123"}}, // a podcast link without an extension
		{Tag: "video", Srcs: []string{"https://example.com/not-media"}},
	}
	got := streamCands("https://example.com/watch", network, media)
	want := []string{"hls https://cdn.example.com/master.m3u8 https://player.example.net/embed/1",
		"video https://cdn.example.com/fallback.mp4 https://example.com/watch",
		"audio https://feeds.example.com/play/123 https://example.com/watch"}
	if len(got) != len(want) {
		t.Fatalf("got %d streams %+v, want %d", len(got), got, len(want))
	}
	for i, s := range got {
		if line := s.Kind + " " + s.URL + " " + s.Referer; line != want[i] {
			t.Errorf("stream %d = %s, want %s", i, line, want[i])
		}
	}
}

func TestPlayerDetection(t *testing.T) {
	base, _ := url.Parse("https://example.com/watch/1")
	for _, c := range []struct {
		name, html string
		want       bool
	}{
		{"video tag", `<video poster="p.jpg"></video>`, true},
		{"embedded player", `<iframe src="https://player.vendor.com/embed/abc"></iframe>`, true},
		{"manifest in a script", `<script>var cfg = {"hls": "https:\/\/cdn.example.com\/a\/master.m3u8"}</script>`, true},
		{"player library", `<script src="/js/video.js/8.0/video.min.js"></script>`, true},
		{"og:video", `<meta property="og:video" content="https://example.com/v.mp4">`, true},
		{"ad iframe", `<iframe src="https://tpc.googlesyndication.com/video/embed"></iframe>`, false},
		{"plain page", `<img src="a.jpg"><iframe src="https://social.example.com/plugins/like.php"></iframe>`, false},
	} {
		if got := extractPage(base, strings.NewReader(c.html)).player; got != c.want {
			t.Errorf("%s: player = %v, want %v", c.name, got, c.want)
		}
	}
}
