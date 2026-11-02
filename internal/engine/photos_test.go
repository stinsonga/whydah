package engine

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestExtractPhotos(t *testing.T) {
	page := `<html><head>
		<title>Fallback title</title>
		<meta property="og:title" content="Harbour &amp; Lights">
		<meta property="og:image" content="/og.jpg">
		<base href="https://cdn.example.com/gallery/">
	</head><body>
		<img src="logo.svg">
		<img src="pixel.gif" width="1" height="1">
		<img src="icon.png" width="48" height="48">
		<img src="a-small.jpg" srcset="a-640.jpg 640w, a-1920.jpg 1920w, a-1280.jpg 1280w">
		<a href="/full/b.jpg"><img src="thumbs/b.jpg"></a>
		<a href="/full/c.png">plain link to an image</a>
		<a href="/about">not an image</a>
		<img data-src="lazy.webp" src="data:image/gif;base64,R0lGOD">
		<picture><source srcset="d.avif 1x, d@2x.avif 2x"><img src="d.jpg"></picture>
		<img src="a-1920.jpg">
	</body></html>`
	base, _ := url.Parse("https://example.com/post/1")
	title, got := extractPhotos(base, strings.NewReader(page))
	if title != "Harbour & Lights" {
		t.Errorf("title = %q", title)
	}
	want := []Photo{
		{Index: 1, URL: "https://cdn.example.com/gallery/a-1920.jpg"},
		{Index: 2, URL: "https://cdn.example.com/full/b.jpg", Thumb: "https://cdn.example.com/gallery/thumbs/b.jpg"},
		{Index: 3, URL: "https://cdn.example.com/full/c.png"},
		{Index: 4, URL: "https://cdn.example.com/gallery/lazy.webp"},
		{Index: 5, URL: "https://cdn.example.com/gallery/d@2x.avif"},
		{Index: 6, URL: "https://cdn.example.com/og.jpg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestDirectImageNotScannedTwice(t *testing.T) {
	// og:image duplicating a page image is only listed once.
	base, _ := url.Parse("https://example.com/")
	_, got := extractPhotos(base, strings.NewReader(`<meta property="og:image" content="https://example.com/x.jpg"><img src="x.jpg">`))
	if len(got) != 1 {
		t.Errorf("got %d photos, want 1: %+v", len(got), got)
	}
}

func TestRangeSpec(t *testing.T) {
	items := func(idx ...int) []Photo {
		var out []Photo
		for _, i := range idx {
			out = append(out, Photo{Index: i})
		}
		return out
	}
	cases := map[string][]Photo{
		"1":          items(1),
		"1-3":        items(1, 2, 3),
		"1-3,7,9-10": items(1, 2, 3, 7, 9, 10),
		"2,4,6":      items(2, 4, 6),
	}
	for want, in := range cases {
		if got := rangeSpec(in); got != want {
			t.Errorf("rangeSpec(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestParseGalleryJSON(t *testing.T) {
	out := `[
	  [2, {"category": "imgur", "album": {"title": "Harbour"}, "user": {"username": "sam"}}],
	  [3, "https://i.imgur.com/a.jpg", {"filename": "a", "extension": "jpg", "width": 951, "height": 1400}],
	  [6, "https://example.com/queued", {}],
	  [3, "https://example.com/icon.svg", {"filename": "icon", "extension": "svg", "width": 20, "height": 20}],
	  [3, "https://i.imgur.com/a.jpg", {"filename": "a", "extension": "jpg"}],
	  [3, "https://i.imgur.com/b.mp4", {"filename": "b", "extension": "mp4"}]
	]`
	md, err := parseGalleryJSON([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if md.Title != "Harbour" || md.Uploader != "sam" || md.Kind != "photos" || md.Source != "gallery-dl" {
		t.Errorf("metadata = %+v", md)
	}
	want := []Photo{
		{Index: 1, URL: "https://i.imgur.com/a.jpg", Name: "a.jpg", Width: 951, Height: 1400},
		{Index: 4, URL: "https://i.imgur.com/b.mp4", Name: "b.mp4", Video: true}, // icon and duplicate skipped, numbering kept
	}
	if !reflect.DeepEqual(md.Photos, want) {
		t.Errorf("photos = %+v", md.Photos)
	}
	if md.Thumbnail != "https://i.imgur.com/a.jpg" {
		t.Errorf("thumbnail = %q", md.Thumbnail)
	}

	_, err = parseGalleryJSON([]byte(`[[-1, {"error": "HttpError", "message": "'404 Not Found' for 'x'"}]]`))
	if err == nil || err.Error() != "'404 Not Found' for 'x'" {
		t.Errorf("error case: %v", err)
	}
}

func TestGalleryTitle(t *testing.T) {
	cases := []struct {
		info         map[string]any
		title, owner string
	}{
		{map[string]any{"category": "wikimediacommons", "page": "Category:Lighthouses_in_Maine", "title": "File:x.jpg"}, "Category:Lighthouses in Maine", ""},
		{map[string]any{"category": "reddit", "title": "My cat", "author": "someone"}, "My cat", "someone"},
		{map[string]any{"category": "instagram", "username": "natgeo"}, "Instagram – natgeo", "natgeo"},
		{map[string]any{}, "Photos", ""},
	}
	for _, c := range cases {
		title, owner := galleryTitle(c.info)
		if title != c.title || owner != c.owner {
			t.Errorf("galleryTitle(%v) = %q, %q; want %q, %q", c.info, title, owner, c.title, c.owner)
		}
	}
}

func TestLargestSrc(t *testing.T) {
	cases := map[string]string{
		"a.jpg 640w, b.jpg 1920w, c.jpg 1280w": "b.jpg",
		"a.jpg 1x, b.jpg 2x":                   "b.jpg",
		"only.jpg":                             "only.jpg",
		"":                                     "",
	}
	for in, want := range cases {
		if got := largestSrc(in); got != want {
			t.Errorf("largestSrc(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPhotoStem(t *testing.T) {
	cases := map[string]string{
		"IMG_0001.jpg":           "IMG_0001",
		"Harbour%20Lights.jpeg":  "Harbour Lights",
		"we?ird*[name].png":      "we_ird__name_",
		"":                       "photo 7",
		"...":                    "photo 7",
		strings.Repeat("x", 100): strings.Repeat("x", 80),
	}
	for in, want := range cases {
		if got := photoStem(in, 7); got != want {
			t.Errorf("photoStem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNotAVideo(t *testing.T) {
	yes := []string{"Unsupported URL: https://example.com", "No video could be found in this tweet", "There is no video in this post", "No media found", "[generic] lighthouse: Unable to download webpage: HTTP Error 401: Unauthorized"}
	no := []string{"Private video. Sign in if you've been granted access", "HTTP Error 403: Forbidden"}
	for _, m := range yes {
		if !notAVideo(errors.New(m)) {
			t.Errorf("notAVideo(%q) = false", m)
		}
	}
	for _, m := range no {
		if notAVideo(errors.New(m)) {
			t.Errorf("notAVideo(%q) = true", m)
		}
	}
}

func TestExtractPhotosFromScripts(t *testing.T) {
	page := `<html><head>
		<script id="__NEXT_DATA__" type="application/json">{"props":{"gallery":[
			{"src":"https:\/\/img.example.com\/p\/one.jpg?w=2000","alt":"one"},
			{"src":"https://img.example.com/p/two.webp"},
			{"src":"//img.example.com/p/three.PNG"}]}}</script>
		<script src="/app.js"></script>
		<script>window.cfg = {logo: "https://example.com/logo.svg", hero: 'https://img.example.com/hero.jpeg'};</script>
		<script type="text/template"><img src="https://img.example.com/tpl.jpg"></script>
	</head><body>
		<noscript><img src="/real.jpg"></noscript>
		<img src="https://img.example.com/p/one.jpg?w=2000">
	</body></html>`
	base, _ := url.Parse("https://example.com/")
	_, got := extractPhotos(base, strings.NewReader(page))
	var urls []string
	for _, p := range got {
		urls = append(urls, p.URL)
	}
	want := []string{
		"https://img.example.com/tpl.jpg",
		"https://example.com/real.jpg",
		"https://img.example.com/p/one.jpg?w=2000",
		"https://img.example.com/p/two.webp",
		"https://img.example.com/p/three.PNG",
		"https://img.example.com/hero.jpeg",
	}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("got\n%q\nwant\n%q", urls, want)
	}
}

func TestImageLinksEnds(t *testing.T) {
	got := imageLinks(`"https://a.example.com/x.jpgfoo/y.png" "https://a.example.com/z.jpg#frag"`)
	want := []string{"https://a.example.com/x.jpgfoo/y.png", "https://a.example.com/z.jpg#frag"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMergeVariants(t *testing.T) {
	in := []Photo{
		{URL: "https://cdn.example.com/a.jpg?w=320", Width: 320, Height: 240},
		{URL: "https://cdn.example.com/b.jpg", Width: 800, Height: 600},
		{URL: "https://cdn.example.com/a.jpg?w=1600", Width: 1600, Height: 1200},
		{URL: "https://cdn.example.com/a.jpg?w=640", Width: 640, Height: 480},
	}
	got := mergeVariants(in)
	want := []Photo{
		{URL: "https://cdn.example.com/a.jpg?w=1600", Width: 1600, Height: 1200, Thumb: "https://cdn.example.com/a.jpg?w=320"},
		{URL: "https://cdn.example.com/b.jpg", Width: 800, Height: 600},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestSameImage(t *testing.T) {
	preview := Photo{URL: "https://images.pexels.com/photos/15038170/pexels-photo-15038170/free-photo-of-clear-sky.jpeg?auto=compress&w=1600", Width: 1600, Height: 2399}
	original := Photo{URL: "https://images.pexels.com/photos/15038170/pexels-photo-15038170.jpeg?cs=srgb&dl=x.jpg", Width: 5464, Height: 8192}
	if !sameImage(preview, original) {
		t.Error("preview and original of one photo not matched")
	}
	// An album's photos share a folder ID and a shape, but they're different photos.
	a := Photo{URL: "https://cdn.example.com/albums/123456/a.jpg", Width: 1500, Height: 1000}
	b := Photo{URL: "https://cdn.example.com/albums/123456/b.jpg", Width: 3000, Height: 2000}
	if sameImage(a, b) {
		t.Error("two album photos matched")
	}
	// Same ID, different shape: a crop, not the same picture.
	crop := Photo{URL: original.URL, Width: 1000, Height: 1000}
	if sameImage(preview, crop) {
		t.Error("crop matched")
	}
	got := mergeVariants([]Photo{preview, {URL: "https://images.pexels.com/photos/1/other.jpg", Width: 800, Height: 600}, original})
	if len(got) != 2 || got[0].URL != original.URL || got[0].Thumb != preview.URL {
		t.Errorf("merge: %+v", got)
	}
}
