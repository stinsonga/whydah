package engine

import (
	"net/url"
	"strings"
	"testing"
)

func TestVideoLinks(t *testing.T) {
	page := `<html><body>
	<nav><a href="/login"><img src="/avatar.png" width="200" height="200"></a>
	     <a href="/"><img src="/logo.jpg"></a></nav>
	<!-- a listing: video-shaped links, one with a duration badge -->
	<a href="/watch/101-harbour-lights" title="Harbour lights"><img src="/t/101.jpg"></a>
	<a href="/watch/101-harbour-lights">Harbour lights</a>
	<div class="card"><a href="/clip/7"><img data-src="/t/7.jpg" alt="Seven"><span class="badge">3:21</span></a></div>
	<!-- same shape as each other, three with play buttons, one plain -->
	<a href="/p/201/tide"><img src="/t/201.jpg"><i class="icon-play"></i> Tide</a>
	<a href="/p/202/wind"><img src="/t/202.jpg"><i class="icon-play"></i> Wind</a>
	<a href="/p/203/rain"><img src="/t/203.jpg"><i class="icon-play"></i> Rain</a>
	<a href="/p/204/fog"><img src="/t/204.jpg"> Fog</a>
	<!-- on a video site; a background-image thumbnail -->
	<a href="https://vimeo.com/12345"><div style="background-image: url('/t/v.jpg')"></div>On Vimeo</a>
	<!-- not videos: an article, a tag page, a link without a thumbnail, an image link -->
	<a href="/articles/how-we-sail"><img src="/t/article.jpg"> How we sail</a>
	<a href="/tags/boats"><img src="/t/tag.jpg"></a>
	<a href="/watch/999">No thumbnail</a>
	<a href="/full/photo.jpg"><img src="/t/photo.jpg"></a>
	</body></html>`
	base, _ := url.Parse("https://example.com/videos")
	scan := extractPage(base, strings.NewReader(page))
	got := videoLinks(base, scan.links)
	want := []LinkedVideo{
		{URL: "https://example.com/watch/101-harbour-lights", Title: "Harbour lights", Thumb: "https://example.com/t/101.jpg"},
		{URL: "https://example.com/clip/7", Title: "Seven", Thumb: "https://example.com/t/7.jpg", Duration: "3:21"},
		{URL: "https://example.com/p/201/tide", Title: "Tide", Thumb: "https://example.com/t/201.jpg"},
		{URL: "https://example.com/p/202/wind", Title: "Wind", Thumb: "https://example.com/t/202.jpg"},
		{URL: "https://example.com/p/203/rain", Title: "Rain", Thumb: "https://example.com/t/203.jpg"},
		{URL: "https://example.com/p/204/fog", Title: "Fog", Thumb: "https://example.com/t/204.jpg"}, // the rest of the listing
		{URL: "https://vimeo.com/12345", Title: "On Vimeo", Thumb: "https://example.com/t/v.jpg"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d:\n got %+v\nwant %+v", i+1, got[i], want[i])
		}
	}
}

func TestLinkShape(t *testing.T) {
	cases := map[string]string{
		"https://example.com/video/123-harbour": "example.com/video/#",
		"https://example.com/p/201/tide":        "example.com/p/#/#",
		"https://example.com/talks/some_title":  "example.com/talks/#",
		"https://example.com/about":             "example.com/about",
	}
	for in, want := range cases {
		u, _ := url.Parse(in)
		if got := linkShape(u); got != want {
			t.Errorf("linkShape(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestVideoLinksFalsePositives(t *testing.T) {
	page := `<html><body>
	<header>
	  <!-- a logo linking to the project (seen on the hls.js demo page) -->
	  <a href="https://github.com/video-dev/hls.js"><img src="/logo.png"></a>
	  <!-- a featured video up top still counts: its address says so -->
	  <a href="/watch/55-launch"><img src="/t/55.jpg"> Launch</a>
	</header>
	<!-- cards whose classes only contain "play" inside other words -->
	<a class="display-card" href="/stories/1-north"><img src="/t/s1.jpg"></a>
	<a class="display-card" href="/stories/2-south"><img src="/t/s2.jpg"></a>
	<a class="display-card" href="/stories/3-east"><img src="/t/s3.jpg"><span class="playlist-count">4 items</span></a>
	<a href="/replay-guide"><img src="/t/r.jpg"><i class="icon-replay"></i></a>
	<!-- video sites: a video counts, a channel or profile doesn't -->
	<a href="https://www.youtube.com/watch?v=abc123"><img src="/t/yt.jpg"> A video</a>
	<a href="https://www.youtube.com/@harbourchannel"><img src="/t/ch.jpg"> Our channel</a>
	<a href="https://vimeo.com/harbourstudio"><img src="/t/vp.jpg"> Vimeo profile</a>
	<!-- a path word joined to an ID counts; joined to a name doesn't -->
	<a href="/video-48213-storm"><img src="/t/48213.jpg"> Storm</a>
	<a href="/video-production-services"><img src="/t/svc.jpg"> Our services</a>
	<!-- a play button counts in the page, but not on a footer icon -->
	<a href="/m/9"><img src="/t/9.jpg"><span class="playButton"></span></a>
	<footer>
	  <a href="https://www.youtube.com/@harbourchannel"><img src="/i/yt.svg"></a>
	  <a href="https://social.example.com/harbour"><img src="/i/social.svg"><i class="icon-play"></i></a>
	</footer>
	</body></html>`
	base, _ := url.Parse("https://example.com/")
	got := videoLinks(base, extractPage(base, strings.NewReader(page)).links)
	var urls []string
	for _, l := range got {
		urls = append(urls, l.URL)
	}
	want := []string{
		"https://example.com/watch/55-launch",
		"https://www.youtube.com/watch?v=abc123",
		"https://example.com/video-48213-storm",
		"https://example.com/m/9",
	}
	if strings.Join(urls, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n  %s\nwant:\n  %s", strings.Join(urls, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestHintClass(t *testing.T) {
	for s, want := range map[string]bool{
		"icon-play": true, "playButton": true, "video-thumb": true, "c-duration": true, "Play video": true,
		"display-card": false, "playlist-item": false, "icon-replay": false, "screenplay": false, "videography": false,
	} {
		if got := hintClass(s); got != want {
			t.Errorf("hintClass(%q) = %v, want %v", s, got, want)
		}
	}
}
