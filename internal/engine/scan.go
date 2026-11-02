package engine

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // registered for DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
	"golang.org/x/net/html"

	"whydah/internal/config"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

// The page scanner is the fallback for sites neither yt-dlp nor gallery-dl
// knows: it reads the page's HTML and collects its images, including image
// links in the data a page's scripts start from. Images a page only fetches
// after loading need the hidden browser instead (render.go).

const (
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	maxPageBytes  = 10 << 20
	maxPhotoBytes = 500 << 20
	maxCandidates = 400
	minPhotoSide  = 160 // images smaller than this both ways are icons, avatars, buttons
	probeBytes    = 256 << 10
	photoWorkers  = 4
)

var httpClient = &http.Client{Timeout: 60 * time.Second}

func get(ctx context.Context, link, referer string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent)
	if referer != "" {
		req.Header.Set("Referer", referer) // many image hosts refuse hotlinks without one
	}
	return httpClient.Do(req)
}

// scanPage lists the photos on a web page, or the image itself for a direct image link.
func scanPage(ctx context.Context, pageURL string) (Metadata, error) {
	resp, err := get(ctx, pageURL, "", http.Header{"Accept": {"text/html,image/*;q=0.9,*/*;q=0.8"}})
	if err != nil {
		return Metadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Metadata{}, statusError{resp.StatusCode, resp.Status}
	}
	md := Metadata{URL: pageURL, Kind: "photos", Source: "page"}
	base := resp.Request.URL // after redirects
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ctype, "image/") {
		name := path.Base(base.Path)
		md.Title = strings.TrimSuffix(name, path.Ext(name))
		md.Photos = []Photo{{Index: 1, URL: base.String(), Name: name}}
		md.Thumbnail = base.String()
		md.direct = true
		return md, nil
	}
	if ctype != "" && ctype != "text/html" && ctype != "application/xhtml+xml" {
		return Metadata{}, errNoPhotos
	}
	scan := extractPage(base, io.LimitReader(resp.Body, maxPageBytes))
	md.Title = firstNonEmpty(scan.title, base.Hostname())
	md.Photos = probePhotos(ctx, pageURL, nil, scan.photos)
	md.Links = videoLinks(base, scan.links)
	md.player = scan.player
	if len(md.Photos) > 0 {
		md.Thumbnail = firstNonEmpty(md.Photos[0].Thumb, md.Photos[0].URL)
	}
	return md, nil
}

// statusError is a page that answered with something other than 200 OK.
type statusError struct {
	code   int
	status string
}

func (e statusError) Error() string { return "the page returned " + e.status }

// extractPhotos collects candidate images from HTML, in page order: <img>
// (largest srcset entry), <picture> sources, and links straight to image
// files, which are usually the full-size version of a thumbnail inside them.
// Then image links in the page's scripts (e.g. Next.js's __NEXT_DATA__),
// which script-built galleries are made from, and last og:image.
func extractPhotos(base *url.URL, r io.Reader) (title string, photos []Photo) {
	scan := extractPage(base, r)
	return scan.title, scan.photos
}

// pageScan is what extractPage finds: the title, candidate photos, and
// links around thumbnails, which may lead to videos (see videoLinks).
type pageScan struct {
	title  string
	photos []Photo
	links  []linkCand
	og     string // the og:image, a poster for a stream the page plays
	player bool   // the page has a video or audio player, whose stream the crow's nest may catch
}

// playerScript spots a player in script text: a stream manifest, or a
// player library's name.
var playerScript = regexp.MustCompile(`(?i)\.m3u8|\.mpd\b|jwplayer|videojs|video\.js|hls\.js|hls\.min\.js|dash\.js|shaka-player|flowplayer|plyr|mediaelement|clappr|bitmovin|theoplayer|brightcove|kaltura|wistia`)

// playerFrame spots an embedded player by its address.
var playerFrame = regexp.MustCompile(`(?i)player|embed|video|stream|/v/|vod|live`)

var styleURL = regexp.MustCompile(`url\(\s*['"]?([^'")]+)`)

func extractPage(base *url.URL, r io.Reader) pageScan {
	var ogTitle, docTitle string
	var photos []Photo
	var links []linkCand
	var ogImages, scriptImages []string
	seen := map[string]bool{}
	add := func(p Photo) {
		if p.URL == "" || seen[p.URL] || len(photos) >= maxCandidates {
			return
		}
		seen[p.URL] = true
		if p.Thumb == p.URL {
			p.Thumb = ""
		}
		p.Index = len(photos) + 1
		photos = append(photos, p)
	}
	type anchor struct {
		href string    // "" unless it links to an image
		used bool      // an <img> inside it already stood for it
		link *linkCand // a link to another page
	}
	var anchors []anchor
	pageLink := func() *linkCand { // the page link being read, if any
		if n := len(anchors); n > 0 {
			return anchors[n-1].link
		}
		return nil
	}
	inTitle := false
	pictureHasSource := false // a <picture>'s <img> is a fallback for its <source>s
	raw := ""                 // inside <script> ("script"), or HTML kept as text: <noscript>, templates ("html")
	player := false
	siteDepth := 0 // inside the page's <header>, <nav> or <footer>

	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		t := z.Token()
		switch tt {
		case html.TextToken:
			switch {
			case inTitle && docTitle == "":
				docTitle = strings.TrimSpace(t.Data)
			case raw == "script":
				scriptImages = append(scriptImages, imageLinks(t.Data)...)
				player = player || playerScript.MatchString(t.Data)
			case raw == "html": // lazy-loading pages often put the real <img> here
				_, inner := extractPhotos(base, strings.NewReader(t.Data))
				for _, p := range inner {
					add(p)
				}
			case raw == "":
				if l := pageLink(); l != nil && len(l.text) < 300 {
					l.text += " " + t.Data
				}
			}
		case html.EndTagToken:
			switch t.Data {
			case "header", "nav", "footer":
				siteDepth = max(0, siteDepth-1)
			case "script", "noscript":
				raw = ""
			case "title":
				inTitle = false
			case "picture":
				pictureHasSource = false
			case "a":
				if n := len(anchors); n > 0 {
					a := anchors[n-1]
					anchors = anchors[:n-1]
					if a.href != "" && !a.used {
						add(Photo{URL: a.href})
					}
					if a.link != nil {
						links = append(links, *a.link)
					}
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			attr := func(name string) string {
				for _, a := range t.Attr {
					if a.Key == name {
						return strings.TrimSpace(a.Val)
					}
				}
				return ""
			}
			if l := pageLink(); l != nil {
				if hintClass(attr("class") + " " + attr("aria-label")) {
					l.hint = true
				}
				if m := styleURL.FindStringSubmatch(attr("style")); m != nil && l.thumb == "" {
					if u := resolve(base, html.UnescapeString(m[1])); u != nil {
						l.thumb = u.String() // a thumbnail drawn as a background
					}
				}
			}
			switch t.Data {
			case "header", "nav", "footer":
				if tt == html.StartTagToken {
					siteDepth++
				}
			case "video", "audio":
				player = true
			case "iframe":
				if u := resolve(base, attr("src")); u != nil && u.Host != base.Host && playerFrame.MatchString(u.Host+u.Path) && !adHost(u.Hostname()) {
					player = true
				}
			case "title":
				inTitle = tt == html.StartTagToken
			case "noscript":
				raw = "html"
			case "script":
				switch typ := strings.ToLower(attr("type")); {
				case strings.Contains(typ, "template") || typ == "text/html":
					raw = "html"
				default: // JavaScript, JSON, ld+json: anything may hold image links
					raw = "script"
				}
				if src := attr("src"); src != "" {
					player = player || playerScript.MatchString(src)
				}
				if tt == html.SelfClosingTagToken || attr("src") != "" {
					raw = ""
				}
			case "picture":
				pictureHasSource = false
			case "base":
				if u := resolve(base, attr("href")); u != nil {
					base = u
				}
			case "meta":
				key := firstNonEmpty(attr("property"), attr("name"))
				switch key {
				case "og:title":
					ogTitle = attr("content")
				case "og:image", "og:image:secure_url", "twitter:image":
					ogImages = append(ogImages, attr("content"))
				case "og:video", "og:video:url", "og:video:secure_url", "twitter:player":
					player = true
				}
			case "a":
				a := anchor{}
				if u := resolve(base, attr("href")); u != nil && imageExt(u.Path) != "" {
					a.href = u.String()
				} else if u != nil {
					a.link = &linkCand{href: u.String(), label: firstNonEmpty(attr("title"), attr("aria-label")),
						hint: hintClass(attr("class")), site: siteDepth > 0}
				}
				if tt == html.StartTagToken {
					anchors = append(anchors, a)
				}
			case "img", "source":
				w, _ := strconv.Atoi(attr("width"))
				h, _ := strconv.Atoi(attr("height"))
				if (w > 0 && w < 32) || (h > 0 && h < 32) || (w > 0 && h > 0 && w < minPhotoSide && h < minPhotoSide) {
					continue // tracking pixels and icons, by their declared size
				}
				src := firstNonEmpty(largestSrc(firstNonEmpty(attr("data-srcset"), attr("srcset"))),
					attr("data-src"), attr("data-lazy-src"), attr("data-original"), attr("data-full"), attr("src"))
				u := resolve(base, src)
				if u == nil || skippable(u) || (t.Data == "img" && pictureHasSource) {
					continue
				}
				if t.Data == "source" {
					pictureHasSource = true
				}
				if l := pageLink(); l != nil && l.thumb == "" { // <img>, or a <picture>'s <source>
					l.thumb, l.alt = u.String(), attr("alt")
				}
				if n := len(anchors); n > 0 && anchors[n-1].href != "" && t.Data == "img" {
					if !anchors[n-1].used { // the link is the full size; this is its thumbnail
						anchors[n-1].used = true
						add(Photo{URL: anchors[n-1].href, Thumb: u.String()})
					}
					continue
				}
				add(Photo{URL: u.String(), Width: w, Height: h})
			}
		}
	}
	for _, link := range scriptImages {
		if u := resolve(base, link); u != nil {
			add(Photo{URL: u.String()})
		}
	}
	var poster string
	for _, og := range ogImages { // usually also in the page; a fallback for pages that aren't galleries
		if u := resolve(base, og); u != nil && !skippable(u) {
			add(Photo{URL: u.String()})
			poster = firstNonEmpty(poster, u.String())
		}
	}
	return pageScan{title: html.UnescapeString(firstNonEmpty(ogTitle, docTitle)), photos: photos, links: links, og: poster, player: player}
}

// imageURL matches absolute or protocol-relative links to image files in
// script text, after JSON escapes are undone. Group 1 is the link; the last
// character only checks that the extension really ends there.
var imageURL = regexp.MustCompile(`(?i)((?:https?:)?//[a-z0-9.-]+\.[a-z]{2,}(?::\d+)?/[^\s"'<>\\(){}\[\]|^` + "`" + `]*?\.(?:jpe?g|png|webp|avif|gif|jfif)(?:[?#][^\s"'<>\\(){}\[\]|^` + "`" + `]*)?)(?:$|[^a-z0-9_])`)

var jsonEscapes = strings.NewReplacer(`\/`, "/", `\u002F`, "/", `\u002f`, "/", `\u0026`, "&", "&amp;", "&")

// imageLinks finds links to image files in a script, e.g. a gallery's JSON.
func imageLinks(script string) []string {
	if !strings.Contains(script, "//") {
		return nil
	}
	var out []string
	for _, m := range imageURL.FindAllStringSubmatch(jsonEscapes.Replace(script), -1) {
		out = append(out, m[1])
	}
	return out
}

func resolve(base *url.URL, ref string) *url.URL {
	if ref == "" || strings.HasPrefix(ref, "data:") {
		return nil
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil
	}
	u.Fragment = ""
	return u
}

func skippable(u *url.URL) bool {
	ext := strings.ToLower(path.Ext(u.Path))
	return ext == ".svg" || ext == ".ico"
}

func imageExt(p string) string {
	switch ext := strings.ToLower(path.Ext(p)); ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".heic", ".bmp", ".tif", ".tiff", ".jfif":
		return ext
	}
	return ""
}

// largestSrc picks the biggest candidate from a srcset ("a.jpg 640w, b.jpg 1280w").
func largestSrc(srcset string) string {
	best, bestSize := "", -1.0
	for _, part := range strings.Split(srcset, ",") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		size := 1.0
		if len(fields) > 1 {
			d := fields[1]
			if v, err := strconv.ParseFloat(strings.TrimRight(d, "wxh"), 64); err == nil {
				size = v
			}
		}
		if size > bestSize {
			best, bestSize = fields[0], size
		}
	}
	return best
}

// probePhotos fetches the start of each candidate to drop broken links,
// non-images and small images, and to learn dimensions. Candidates it can't
// check in time are kept, since dropping a real photo is worse than showing an icon.
func probePhotos(ctx context.Context, referer string, jar cookieJar, cands []Photo) []Photo {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	keep := make([]bool, len(cands))
	var wg sync.WaitGroup
	next := make(chan int)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				keep[i] = probe(ctx, referer, jar, &cands[i])
			}
		}()
	}
	for i := range cands {
		next <- i
	}
	close(next)
	wg.Wait()
	var kept []Photo
	for i, p := range cands {
		if keep[i] {
			kept = append(kept, p)
		}
	}
	out := mergeVariants(kept)
	for i := range out {
		out[i].Index = i + 1
	}
	return out
}

// mergeVariants keeps only the largest of the sizes of one picture, at the
// position of the first: links that differ just in their query, which CDNs
// often use for sizes (?w=320, ?w=1600), and links that sameImage matches.
func mergeVariants(photos []Photo) []Photo {
	key := func(p Photo) string {
		if u, err := url.Parse(p.URL); err == nil && u.RawQuery != "" {
			return u.Host + u.Path
		}
		return p.URL
	}
	best := map[string]int{} // key -> index into out
	var out []Photo
	for _, p := range photos {
		k := key(p)
		i, ok := best[k]
		if !ok {
			best[k] = len(out)
			out = append(out, p)
			continue
		}
		out[i] = larger(out[i], p)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if sameImage(out[i], out[j]) {
				out[i] = larger(out[i], out[j])
				out = append(out[:j], out[j+1:]...)
				j--
			}
		}
	}
	return out
}

func larger(a, b Photo) Photo {
	if b.Width*b.Height > a.Width*a.Height {
		b.Thumb = firstNonEmpty(a.Thumb, a.URL)
		return b
	}
	return a
}

var idToken = regexp.MustCompile(`\d{5,}`)

// sameImage reports whether two links look like one picture at two sizes,
// e.g. a preview ".../photos/15038170/pexels-photo-15038170/view.jpeg?w=1600"
// and the original ".../photos/15038170/pexels-photo-15038170.jpeg": same
// host and shape, and an image ID in common that one of them is named after.
// Not just a shared folder: an album's photos often have the same shape too.
func sameImage(a, b Photo) bool {
	if a.Width == 0 || a.Height == 0 || b.Width == 0 || b.Height == 0 {
		return false
	}
	ra, rb := float64(a.Width)/float64(a.Height), float64(b.Width)/float64(b.Height)
	if ra/rb > 1.015 || rb/ra > 1.015 {
		return false
	}
	ua, err1 := url.Parse(a.URL)
	ub, err2 := url.Parse(b.URL)
	if err1 != nil || err2 != nil || ua.Host != ub.Host {
		return false
	}
	nameA, nameB := path.Base(ua.Path), path.Base(ub.Path)
	for _, id := range idToken.FindAllString(ua.Path, -1) {
		if strings.Contains(ub.Path, id) && (strings.Contains(nameA, id) || strings.Contains(nameB, id)) {
			return true
		}
	}
	return false
}

func probe(ctx context.Context, referer string, jar cookieJar, p *Photo) bool {
	if p.measured { // the hidden browser already knows its real size
		return p.Width >= minPhotoSide || p.Height >= minPhotoSide
	}
	header := http.Header{"Range": {fmt.Sprintf("bytes=0-%d", probeBytes-1)}}
	if u, err := url.Parse(p.URL); err == nil {
		jar.apply(header, u)
	}
	resp, err := get(ctx, p.URL, referer, header)
	if err != nil {
		return ctx.Err() != nil // timed out: keep; refused or unreachable: drop
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(ctype, "image/") && ctype != "application/octet-stream" && ctype != "" {
		return false // an HTML page or similar behind an image-looking link
	}
	if ctype == "image/svg+xml" {
		return false
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, probeBytes))
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		p.Width, p.Height = cfg.Width, cfg.Height
	}
	if p.Width > 0 && p.Height > 0 && p.Width < minPhotoSide && p.Height < minPhotoSide {
		return false
	}
	return true // includes AVIF/HEIC, whose size we can't read
}

// runPagePhotos downloads the chosen photos itself, a few at a time, as
// "NNN - name.ext" so a retry can skip the ones already saved. Photos found
// by the crow's nest are fetched with the same cookies it used.
func (e *Engine) runPagePhotos(ctx context.Context, j *Job, paths tools.Paths, s config.Settings) error {
	dir := photoDir(s, j)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	items := j.photos.Items
	pad := max(2, len(strconv.Itoa(j.photos.Total)))
	saved := map[string]bool{} // names without extension, from an earlier attempt
	if entries, err := os.ReadDir(dir); err == nil {
		for _, de := range entries {
			if name := de.Name(); !strings.HasSuffix(name, ".part") {
				saved[strings.TrimSuffix(name, filepath.Ext(name))] = true
			}
		}
	}
	var jar cookieJar
	if j.photos.Source == "browser" && s.BrowserCookies != "" {
		var err error
		if jar, err = siteCookies(ctx, paths.YtDlp, s.BrowserCookies, j.URL); err != nil {
			slog.Warn("couldn't read browser cookies", "browser", s.BrowserCookies, "err", err)
		}
	}
	slog.Info("photos starting", "url", j.URL, "source", j.photos.Source, "files", len(items))
	e.mutate(j, func(j *Job) { j.State, j.FilePath = Downloading, dir })

	var mu sync.Mutex
	var failed int
	var firstErr error
	next := make(chan Photo)
	var wg sync.WaitGroup
	for range photoWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range next {
				n, err := savePhoto(ctx, j.URL, jar, dir, fmt.Sprintf("%0*d - ", pad, p.Index), p, saved)
				if err != nil && ctx.Err() == nil {
					slog.Warn("photo failed", "url", p.URL, "err", err)
					mu.Lock()
					failed++
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				e.mutate(j, func(j *Job) {
					j.Files++
					j.Downloaded += n
					j.Percent = float64(j.Files) * 100 / float64(max(j.FileTotal, 1))
				})
			}
		}()
	}
feed:
	for _, p := range items {
		select {
		case next <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case failed == len(items):
		return firstErr
	case failed > 0:
		return i18n.New("error.photos_failed", "count", strconv.Itoa(failed), "total", strconv.Itoa(len(items))).WithDetail(firstErr.Error())
	}
	return nil
}

// savePhoto downloads one photo into dir, returning the bytes written. A file
// in saved (same number and name, any extension) counts as already done.
func savePhoto(ctx context.Context, referer string, jar cookieJar, dir, prefix string, p Photo, saved map[string]bool) (int64, error) {
	u, err := url.Parse(p.URL)
	if err != nil {
		return 0, err
	}
	stem := photoStem(firstNonEmpty(p.Name, path.Base(u.Path)), p.Index)
	if saved[prefix+stem] {
		return 0, nil
	}
	header := http.Header{}
	jar.apply(header, u)
	resp, err := get(ctx, p.URL, referer, header)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: %s", path.Base(u.Path), resp.Status)
	}
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ctype, "text/") {
		return 0, fmt.Errorf("%s: the site sent a web page instead of the image", path.Base(u.Path))
	}
	ext := firstNonEmpty(extForType(ctype), imageExt(firstNonEmpty(p.Name, u.Path)), ".jpg")
	dest := filepath.Join(dir, prefix+stem+ext)
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxPhotoBytes))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, dest)
}

// photoStem makes a filename (without extension) from a URL's last path part.
func photoStem(name string, index int) string {
	if dec, err := url.PathUnescape(name); err == nil {
		name = dec
	}
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*[]`, r) {
			return '_'
		}
		return r
	}, name)
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	if name = strings.Trim(name, " ."); name == "" {
		name = "photo " + strconv.Itoa(index)
	}
	return name
}

func extForType(ctype string) string {
	return map[string]string{
		"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
		"image/avif": ".avif", "image/heic": ".heic", "image/bmp": ".bmp", "image/tiff": ".tif",
	}[ctype]
}
