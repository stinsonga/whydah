package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"whydah/internal/config"
	"whydah/internal/events"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

// Linked videos: listing pages show thumbnails that lead to a page with the
// video. Scanning a page notes which thumbnail links look like that (cheap,
// no requests); checking them runs yt-dlp on each linked page, only when
// asked, and the ones with a video come back as a playlist to salvage.

// LinkedVideo is a thumbnail link that looks like it leads to a video.
type LinkedVideo struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Thumb    string `json:"thumb,omitempty"`
	Duration string `json:"duration,omitempty"` // the badge shown on the thumbnail, e.g. "12:34"
}

// LinksProgress is published while linked pages are checked.
type LinksProgress struct {
	URL   string `json:"url"` // the page the links came from
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// linkCand is a link around a thumbnail, as found on the page.
type linkCand struct {
	href  string // resolved
	text  string // the link's text, including any duration badge
	label string // title or aria-label
	alt   string // the thumbnail's alt text
	thumb string
	hint  bool // something inside is named like a play button, video or duration
	site  bool // in the page's header, nav or footer: logos, profiles, menus
}

const maxLinkedVideos = 200

var (
	durationRe = regexp.MustCompile(`\b(?:\d{1,2}:)?\d{1,2}:\d{2}\b`)
	navPath    = regexp.MustCompile(`(?i)/(log-?in|sign-?in|sign-?up|register|account|settings|tags?|categor(y|ies)|search|privacy|terms|about|contact|help|cart|checkout)(/|$|[-_.?])`)
	spaceRe    = regexp.MustCompile(`\s+`)
	digitSeg   = regexp.MustCompile(`\d`)

	// Words in a video page's address: a path segment that is one ("/watch",
	// "/talks/..."), or one joined to an ID ("/video-48213", "clip_9").
	videoWords = map[string]bool{"watch": true, "video": true, "videos": true, "v": true, "embed": true, "clip": true, "clips": true,
		"reel": true, "reels": true, "shorts": true, "episode": true, "episodes": true, "movie": true, "movies": true, "film": true,
		"films": true, "player": true, "vod": true, "stream": true, "talk": true, "talks": true, "trailer": true, "trailers": true,
		"lecture": true, "lectures": true}
	wordWithID = regexp.MustCompile(`^([a-z]+)[-_].*\d`)

	// Video sites, with the addresses of their videos (not channels or profiles).
	videoHosts = map[string]*regexp.Regexp{
		"youtube.com":     regexp.MustCompile(`^/(watch|shorts/|embed/|live/)`),
		"youtu.be":        regexp.MustCompile(`^/[\w-]{6,}`),
		"vimeo.com":       regexp.MustCompile(`^/(\d+|channels/[^/]+/\d+|.+/video/\d+)`),
		"dailymotion.com": regexp.MustCompile(`^/video/`),
		"twitch.tv":       regexp.MustCompile(`^/(videos/|[^/]+/clip/)`),
		"tiktok.com":      regexp.MustCompile(`/video/\d`),
		"streamable.com":  regexp.MustCompile(`^/\w+$`),
		"rumble.com":      regexp.MustCompile(`^/v\w+-`),
		"bilibili.com":    regexp.MustCompile(`^/video/`),
		"odysee.com":      regexp.MustCompile(`^/@[^/]+/[^/]+`),
		"facebook.com":    regexp.MustCompile(`^/(watch|reel/|[^/]+/videos/)`),
		"instagram.com":   regexp.MustCompile(`^/(reel|reels|tv)/`),
	}

	// Words in a class or label that mark a play button or a duration
	// badge: whole words, so "display" and "playlist" don't count.
	hintWords = map[string]bool{"play": true, "video": true, "duration": true, "runtime": true}
	hintJoint = regexp.MustCompile(`^(play|video)(btn|button|icon|overlay|badge|thumb|thumbnail|card|item|link|time|length)$|^(duration|runtime)`)
	wordSplit = regexp.MustCompile(`[^a-z0-9]+`)
	camelCase = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

func hintClass(s string) bool {
	for _, w := range wordSplit.Split(strings.ToLower(camelCase.ReplaceAllString(s, "$1 $2")), -1) {
		if hintWords[w] || hintJoint.MatchString(w) {
			return true
		}
	}
	return false
}

// videoPathLike reports whether a link's path looks like a video page's.
func videoPathLike(p string) bool {
	for _, seg := range strings.Split(strings.ToLower(p), "/") {
		seg = strings.TrimSuffix(seg, path.Ext(seg)) // "watch.php"
		if videoWords[seg] {
			return true
		}
		if m := wordWithID.FindStringSubmatch(seg); m != nil && videoWords[m[1]] {
			return true
		}
	}
	return false
}

// videoLinks picks the thumbnail links that look like they lead to videos.
// A link counts if its address looks like a video page, it goes to a video
// site, its thumbnail shows a duration, or it has a play button; and then
// the rest of a listing too: links shaped like three or more that count.
func videoLinks(page *url.URL, cands []linkCand) []LinkedVideo {
	type scored struct {
		LinkedVideo
		shape string
		ok    bool
	}
	pageURL := strings.TrimSuffix(page.String(), "/")
	byURL := map[string]int{}
	var list []scored
	for _, c := range cands {
		u, err := url.Parse(c.href)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || imageExt(u.Path) != "" {
			continue
		}
		u.Fragment = ""
		link := u.String()
		if strings.TrimSuffix(link, "/") == pageURL || navPath.MatchString(u.Path) || u.Path == "" || u.Path == "/" && u.RawQuery == "" {
			continue
		}
		dur := durationRe.FindString(c.text + " " + c.label)
		title := strings.TrimSpace(spaceRe.ReplaceAllString(strings.Replace(c.text, dur, "", 1), " "))
		title = firstNonEmpty(strings.TrimSpace(c.label), title, strings.TrimSpace(c.alt))
		// In the site's header, nav or footer only an address or a duration
		// will do: links there with a logo or an icon are rarely videos.
		strong := dur != "" || videoPathLike(u.Path) || u.Query().Get("v") != ""
		ok := strong || !c.site && (c.hint || onVideoHost(u))
		if i, seen := byURL[link]; seen { // a listing often links the thumbnail and the title separately
			s := &list[i]
			s.ok = s.ok || ok
			s.Title = firstNonEmpty(s.Title, title)
			s.Thumb = firstNonEmpty(s.Thumb, c.thumb)
			s.Duration = firstNonEmpty(s.Duration, dur)
			if !c.site {
				s.shape = linkShape(u) // also linked from the page itself
			}
			continue
		}
		// Menus' links make listings of their own, so a row of profile
		// links can't pass for the rest of a row of videos.
		shape := linkShape(u)
		if c.site {
			shape = "site:" + shape
		}
		byURL[link] = len(list)
		list = append(list, scored{LinkedVideo{URL: link, Title: title, Thumb: c.thumb, Duration: dur}, shape, ok})
	}
	listing := map[string]int{}
	for _, s := range list {
		if s.ok {
			listing[s.shape]++
		}
	}

	var out []LinkedVideo
	for _, s := range list {
		if (s.ok || listing[s.shape] >= 3) && s.Thumb != "" && len(out) < maxLinkedVideos {
			if s.Title == "" {
				s.Title = strings.ReplaceAll(path.Base(strings.TrimSuffix(s.URL, "/")), "-", " ")
			}
			out = append(out, s.LinkedVideo)
		}
	}
	return out
}

// onVideoHost reports whether u is a video on a video site; a channel or
// profile there isn't.
func onVideoHost(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	for h, videoPage := range videoHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return videoPage.MatchString(u.Path)
		}
	}
	return false
}

// linkShape is a link's host and path with the parts that vary between
// items (anything with a digit, the last segment) generalized:
// "/video/123-harbour" and "/video/456-lights" are both "/video/#".
func linkShape(u *url.URL) string {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range segs {
		if digitSeg.MatchString(s) || i == len(segs)-1 && len(segs) > 1 {
			segs[i] = "#"
		}
	}
	return u.Host + "/" + strings.Join(segs, "/")
}

// LinkedPlaylist checks a page's linked videos and returns the ones that are
// videos as a playlist named after the page, to pick from and salvage.
func (e *Engine) LinkedPlaylist(ctx context.Context, pageURL, title string, links []LinkedVideo) (Metadata, error) {
	entries, err := e.CheckLinks(ctx, pageURL, links)
	if err != nil {
		return Metadata{}, explainErr(err, pageURL, e.settings.Get())
	}
	if len(entries) == 0 {
		return Metadata{}, i18n.New("error.linked_none")
	}
	return Metadata{URL: pageURL, Title: firstNonEmpty(title, "Linked videos"), Kind: "playlist", Source: "links",
		Entries: entries, Thumbnail: entries[0].Thumbnail}, nil
}

// CheckLinks runs yt-dlp on each linked page, a few at a time, and returns
// the ones with a video as playlist entries in page order. Pages that don't
// answer in time are left out.
func (e *Engine) CheckLinks(ctx context.Context, pageURL string, links []LinkedVideo) ([]Entry, error) {
	paths, err := e.tools.Wait(ctx)
	if err != nil {
		return nil, err
	}
	s := e.settings.Get()
	if len(links) > maxLinkedVideos {
		links = links[:maxLinkedVideos]
	}
	const batch, workers = 6, 4 // several links per yt-dlp run saves its start-up time
	found := make(map[string]Entry)
	var mu sync.Mutex
	done := 0
	publish := func() {
		e.hub.Publish(events.LinksProgress, LinksProgress{URL: pageURL, Done: done, Total: len(links)})
	}
	publish()

	jobs := make(chan []LinkedVideo)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for group := range jobs {
				answered := 0
				e.checkBatch(ctx, paths, s, group, func(en Entry) {
					mu.Lock()
					found[en.URL] = en
					answered++
					done++
					publish()
					mu.Unlock()
				})
				mu.Lock()
				done += len(group) - answered // the ones that weren't videos
				publish()
				mu.Unlock()
			}
		}()
	}
feed:
	for i := 0; i < len(links); i += batch {
		select {
		case jobs <- links[i:min(i+batch, len(links))]:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil && len(found) == 0 {
		return nil, ctx.Err()
	}
	var out []Entry
	for _, l := range links {
		if en, ok := found[l.URL]; ok {
			en.Index = len(out) + 1
			en.Title = firstNonEmpty(en.Title, l.Title)
			en.Thumbnail = firstNonEmpty(en.Thumbnail, l.Thumb)
			out = append(out, en)
		}
	}
	return out, nil
}

// checkBatch asks one yt-dlp run about a few links, passing each video to found as it's reported.
func (e *Engine) checkBatch(ctx context.Context, paths tools.Paths, s config.Settings, links []LinkedVideo, found func(Entry)) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	args := append(commonArgs(paths, s), "-j", "--no-playlist", "--flat-playlist", "--ignore-errors", "--socket-timeout", "15", "--")
	for _, l := range links {
		args = append(args, l.URL)
	}
	release := e.tools.Use()
	defer release()
	cmd := tools.Command(ctx, paths.YtDlp, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	asked := map[string]bool{}
	for _, l := range links {
		asked[l.URL] = true
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024) // a video's JSON lists every format
	for sc.Scan() {
		var info struct {
			ytInfo
			OriginalURL string `json:"original_url"` // the link as given
		}
		if json.Unmarshal(sc.Bytes(), &info) != nil || info.Type == "url" || info.Type == "playlist" || imageExt("."+info.Ext) != "" {
			continue // a playlist's entries, or a picture rather than a video
		}
		link := firstNonEmpty(info.OriginalURL, info.WebpageURL)
		if !asked[link] {
			continue
		}
		delete(asked, link) // once each, so progress can't run past the total
		found(Entry{URL: link, Title: info.Title, Duration: info.Duration,
			DurationString: formatDuration(info.Duration), Thumbnail: info.thumbnail()})
	}
	_, _ = io.Copy(io.Discard, out)
	_ = cmd.Wait() // failures for individual links are expected: those just aren't videos
}
