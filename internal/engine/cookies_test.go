package engine

import (
	"net/url"
	"strings"
	"testing"

	"whydah/internal/config"
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

// The sign-in browser's cookies, read over CDP, reach yt-dlp as a cookies.txt.
func TestCookiesFromCDP(t *testing.T) {
	jar := fromCDP([]cdpCookie{
		{Name: "sid", Value: "abc", Domain: ".Example.com", Path: "/", Expires: 2000000000.5, Secure: true},
		{Name: "sess", Value: "xyz", Domain: "www.example.com", Path: "/account", Expires: -1, HTTPOnly: true},
		{Name: "nope", Value: "1", Domain: ".other.com", Path: "/", Expires: -1},
		{Name: "bad", Value: "a\tb", Domain: "example.com", Path: "/", Expires: -1},
	})
	if c := jar[0]; c.Domain != "example.com" || !c.Sub || c.Expires != 2000000000 {
		t.Errorf("domain cookie converted as %+v", c)
	}
	if c := jar[1]; c.Sub || c.Expires != 0 || !c.HTTPOnly {
		t.Errorf("host-only session cookie converted as %+v", c)
	}
	if got := jar.forSite("example.com"); len(got) != 3 {
		t.Errorf("forSite kept %d cookies, want 3", len(got))
	}

	var b strings.Builder
	if err := jar.write(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "bad") {
		t.Error("wrote a cookie with a tab in it, which would break the file")
	}
	back := parseCookies(strings.NewReader(b.String()), "example.com")
	if len(back) != 2 || back[0] != jar[0] || back[1] != jar[1] {
		t.Errorf("read back %+v, want %+v", back, jar[:2])
	}
}

func TestCookieArgs(t *testing.T) {
	cases := []struct {
		browser, file string
		want          string
	}{
		{"", "", ""},
		{"firefox", "", "--cookies-from-browser firefox"},
		{config.OwnBrowser, "", ""}, // not signed in anywhere yet
		{config.OwnBrowser, "/x/cookies.txt", "--cookies /x/cookies.txt"},
	}
	for _, c := range cases {
		if got := strings.Join(cookieArgs(config.Settings{BrowserCookies: c.browser}, c.file), " "); got != c.want {
			t.Errorf("cookieArgs(%q, %q) = %q, want %q", c.browser, c.file, got, c.want)
		}
	}
}
