package engine

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseCookies(t *testing.T) {
	file := "# Netscape HTTP Cookie File\n" +
		".example.com\tTRUE\t/\tTRUE\t2000000000\tsid\tabc\n" +
		"#HttpOnly_www.example.com\tFALSE\t/account\tTRUE\t0\tsess\txyz\n" +
		"img.example.com\tFALSE\t/\tFALSE\t0\timg\t1\n" +
		".other.com\tTRUE\t/\tFALSE\t0\tnope\t1\n" +
		"malformed line\n"
	jar := parseCookies(strings.NewReader(file), site("www.example.com"))
	if len(jar) != 3 {
		t.Fatalf("kept %d cookies, want 3: %+v", len(jar), jar)
	}
	if !jar[1].HTTPOnly || jar[1].Sub || jar[1].Domain != "www.example.com" {
		t.Errorf("HttpOnly cookie parsed as %+v", jar[1])
	}
	cases := map[string]string{
		"https://www.example.com/account/x": "sid=abc; sess=xyz",
		"https://www.example.com/":          "sid=abc",
		"https://img.example.com/a.jpg":     "sid=abc; img=1",
		"http://img.example.com/a.jpg":      "img=1", // secure cookies only over https
		"https://cdn.example.com/a.jpg":     "sid=abc",
		"https://other.com/":                "",
	}
	for link, want := range cases {
		u, _ := url.Parse(link)
		if got := jar.header(u); got != want {
			t.Errorf("header(%s) = %q, want %q", link, got, want)
		}
	}
}

func TestSite(t *testing.T) {
	cases := map[string]string{
		"www.example.com":   "example.com",
		"example.com":       "example.com",
		"a.b.example.co.uk": "example.co.uk",
		"www.abc.net.au":    "abc.net.au",
		"localhost":         "localhost",
	}
	for host, want := range cases {
		if got := site(host); got != want {
			t.Errorf("site(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestRenderedCandidates(t *testing.T) {
	base, _ := url.Parse("https://example.com/gallery")
	page := renderedPage{
		Items: []renderedItem{
			// Shown at 800 wide from a srcset that has a 1600 version.
			{Kind: "img", Src: "/a-800.jpg", Srcset: "/a-800.jpg 800w, /a-1600.jpg 1600w", W: 800, H: 600},
			// Loaded at its own size: measured.
			{Kind: "img", Src: "/b.jpg", W: 1200, H: 900},
			// A thumbnail linking to the original.
			{Kind: "img", Src: "/c-thumb.jpg", W: 200, H: 150, Href: "/c.jpg"},
			{Kind: "bg", Src: "https://example.com/hero.webp"},
		},
		// The rendered HTML has the same images (sizes must not come back as
		// separate photos) plus one only a page scan finds.
		HTML: `<img src="/a-800.jpg" srcset="/a-800.jpg 800w, /a-1600.jpg 1600w"><img src="/b.jpg"><noscript><img src="/d.jpg"></noscript>`,
	}
	_, static := extractPhotos(base, strings.NewReader(page.HTML))
	got := renderedCandidates(base, page, static, []string{"https://example.com/b.jpg", "https://example.com/e.png"})
	var urls []string
	for _, p := range got {
		urls = append(urls, strings.TrimPrefix(p.URL, "https://example.com"))
	}
	want := "/a-1600.jpg /b.jpg /c.jpg /hero.webp /d.jpg /e.png"
	if strings.Join(urls, " ") != want {
		t.Errorf("got %v, want %s", urls, want)
	}
	if !got[1].measured || got[1].Width != 1200 || got[0].measured || got[0].Thumb != "https://example.com/a-800.jpg" {
		t.Errorf("sizes: %+v %+v", got[0], got[1])
	}
}
