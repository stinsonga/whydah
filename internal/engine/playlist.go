package engine

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"whydah/internal/events"
)

// Entry is one item of an inspected playlist.
type Entry struct {
	Index          int     `json:"index"` // 1-based
	URL            string  `json:"url"`
	Title          string  `json:"title"`
	Duration       float64 `json:"duration"`
	DurationString string  `json:"duration_string"`
	Thumbnail      string  `json:"thumbnail"`
	StreamFrom
}

// StreamFrom is set on an entry that's a stream the crow's nest caught on a
// linked page, rather than a page yt-dlp reads: see Job.Referer.
type StreamFrom struct {
	Referer string `json:"referer,omitempty"`
	Origin  string `json:"origin,omitempty"`
	Page    string `json:"page,omitempty"`
}

// Item is a playlist entry chosen for download.
type Item struct {
	Index     int    `json:"index"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Thumbnail string `json:"thumbnail"`
	StreamFrom
}

// ytInfo is the subset of yt-dlp's -J output we use, for videos, playlists
// and (with --flat-playlist) playlist entries.
type ytInfo struct {
	Type           string                 `json:"_type"`
	URL            string                 `json:"url"`
	WebpageURL     string                 `json:"webpage_url"`
	Ext            string                 `json:"ext"`
	Title          string                 `json:"title"`
	Uploader       string                 `json:"uploader"`
	Channel        string                 `json:"channel"`
	Duration       float64                `json:"duration"`
	Thumbnail      string                 `json:"thumbnail"`
	Thumbnails     []struct{ URL string } `json:"thumbnails"`
	Filesize       int64                  `json:"filesize"`
	FilesizeApprox int64                  `json:"filesize_approx"`
	Entries        []ytInfo               `json:"entries"`
}

func (i ytInfo) thumbnail() string {
	if i.Thumbnail != "" {
		return i.Thumbnail
	}
	if n := len(i.Thumbnails); n > 0 {
		return i.Thumbnails[n-1].URL // yt-dlp orders thumbnails worst to best
	}
	return ""
}

// playlistEntries keeps the entries that have a downloadable URL.
func playlistEntries(raw []ytInfo) []Entry {
	var out []Entry
	for _, e := range raw {
		link := e.URL
		if !strings.HasPrefix(link, "http") {
			link = e.WebpageURL
		}
		if !strings.HasPrefix(link, "http") {
			continue // unavailable/private items come through without a URL
		}
		index := len(out) + 1
		title := e.Title
		if title == "" {
			title = titleFromURL(link, index)
		}
		out = append(out, Entry{
			Index:          index,
			URL:            link,
			Title:          title,
			Duration:       e.Duration,
			DurationString: formatDuration(e.Duration),
			Thumbnail:      e.thumbnail(),
		})
	}
	return out
}

// titleFromURL is a placeholder title for entries listed without one (some
// sites' sets); the real title replaces it once the item downloads.
func titleFromURL(link string, index int) string {
	u, err := url.Parse(link)
	if err != nil {
		return fmt.Sprintf("Item %d", index)
	}
	slug := path.Base(u.Path)
	if strings.Trim(slug, "0123456789") == "" || slug == "/" || slug == "." {
		return fmt.Sprintf("Item %d", index) // numeric API ids say nothing useful
	}
	return strings.NewReplacer("-", " ", "_", " ").Replace(slug)
}

// watchPlaylistURL returns the playlist a watch link was opened from (its
// &list= parameter), or "", on the site whose links work that way.
func watchPlaylistURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "youtu.be" && host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") {
		return ""
	}
	list := u.Query().Get("list")
	if list == "" {
		return ""
	}
	return "https://www.youtube.com/playlist?list=" + url.QueryEscape(list)
}

// DownloadPlaylist queues items as one group, saved into a folder named
// after the playlist with files numbered by playlist position. total is the
// playlist's full length, used to pad the numbers.
func (e *Engine) DownloadPlaylist(title string, total int, audio bool, items []Item) []Job {
	e.mu.Lock()
	e.seq++
	group := "g" + strconv.Itoa(e.seq)
	e.mu.Unlock()

	title = firstNonEmpty(strings.TrimSpace(title), "Playlist")
	pad := 2
	for _, it := range items {
		total = max(total, it.Index)
	}
	pad = max(pad, len(strconv.Itoa(total)))

	jobs := make([]Job, 0, len(items))
	for _, it := range items {
		jobs = append(jobs, e.Download(Request{
			URL:        it.URL,
			Audio:      audio,
			Title:      it.Title,
			Thumbnail:  it.Thumbnail,
			Referer:    it.Referer,
			Origin:     it.Origin,
			Page:       it.Page,
			group:      group,
			groupTitle: title,
			index:      it.Index,
			groupSize:  len(items),
			pad:        pad,
		}))
	}
	return jobs
}

// CancelGroup cancels every unfinished job in a playlist group.
func (e *Engine) CancelGroup(group string) int {
	e.mu.Lock()
	var cancels []context.CancelFunc
	for id, j := range e.jobs {
		if j.Group == group {
			if c, ok := e.cancels[id]; ok {
				cancels = append(cancels, c)
			}
		}
	}
	e.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	e.wake()
	return len(cancels)
}

// Retry re-queues a failed or canceled job, keeping its ID and group. Photo
// jobs skip files already saved, so a retry only fetches what's missing.
func (e *Engine) Retry(id string) (Job, bool) {
	e.mu.Lock()
	j, ok := e.jobs[id]
	_, active := e.cancels[id]
	if !ok || active || (j.State != Failed && j.State != Canceled) {
		e.mu.Unlock()
		return Job{}, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancels[id] = cancel
	j.reset()
	j.Created = time.Now()
	snap := *j
	e.wg.Add(1)
	e.mu.Unlock()

	e.hub.Publish(events.JobUpdated, snap)
	e.queueChanged()
	go e.run(ctx, j)
	return snap, true
}

// outputTemplate is where yt-dlp saves a job: named by the file name
// template for a single video or stream, or numbered in the playlist's folder.
func outputTemplate(base, nameTemplate string, j *Job) string {
	base = escapeTemplate(base)
	if j.Group == "" {
		return filepath.Join(base, ytNameTemplate(nameTemplate)+".%(ext)s")
	}
	name := "%(title)s.%(ext)s"
	if j.Index > 0 {
		name = fmt.Sprintf("%0*d - ", j.pad, j.Index) + name
	}
	return filepath.Join(base, escapeTemplate(sanitizeDirName(j.GroupTitle)), name)
}

// nameFields are the file name template's tokens as yt-dlp fields, with
// fallbacks so none comes out as "NA": a stream has no uploader, so it gets
// the site's name.
var nameFields = map[string]string{
	"title":    "%(title)s",
	"uploader": "%(uploader,channel,uploader_id,webpage_url_domain)s",
	"date":     "%(upload_date>%Y-%m-%d,release_date>%Y-%m-%d|)s",
	"site":     "%(webpage_url_domain)s",
	"id":       "%(id)s",
}

var nameToken = regexp.MustCompile(`\{(\w+)\}`)

// ytNameTemplate turns a file name template ("{uploader}/{title}") into
// yt-dlp's output template, with the text around the tokens kept literally.
func ytNameTemplate(t string) string {
	if t == "" {
		t = "{title}"
	}
	var b strings.Builder
	last := 0
	for _, m := range nameToken.FindAllStringSubmatchIndex(t, -1) {
		b.WriteString(escapeTemplate(t[last:m[0]]))
		if f, ok := nameFields[t[m[2]:m[3]]]; ok {
			b.WriteString(f)
		} else {
			b.WriteString(escapeTemplate(t[m[0]:m[1]]))
		}
		last = m[1]
	}
	b.WriteString(escapeTemplate(t[last:]))
	return filepath.FromSlash(strings.ReplaceAll(b.String(), "\\", "/"))
}

// playlistMetadataArgs tag playlist items with the playlist as album and
// their position as track number, so music players keep albums together.
func playlistMetadataArgs(j *Job) []string {
	if j.Group == "" {
		return nil
	}
	args := []string{"--parse-metadata", literalMetadata(j.GroupTitle) + ":%(album)s"}
	if j.Index > 0 {
		args = append(args, "--parse-metadata", literalMetadata(strconv.Itoa(j.Index))+":%(track_number)s")
	}
	return args
}

// literalMetadata is s as the FROM of --parse-metadata FROM:TO. A FROM that
// looks like a bare word is read as a field name, so an always-empty field
// first forces it to be a template; then % is doubled and the FROM:TO
// separator colon escaped.
func literalMetadata(s string) string {
	return "%(__literal|)s" + strings.ReplaceAll(escapeTemplate(s), ":", `\:`)
}

// escapeTemplate makes s literal inside a yt-dlp output template.
func escapeTemplate(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// sanitizeDirName makes a playlist title safe as a folder name on every OS.
func sanitizeDirName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	s = strings.Trim(s, " .")
	if s == "" {
		return "Playlist"
	}
	return s
}
