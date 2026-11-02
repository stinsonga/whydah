package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"whydah/internal/config"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

// Photo is one image (or a video clip inside a gallery) found by inspection.
type Photo struct {
	Index  int    `json:"index"` // 1-based position in the gallery
	URL    string `json:"url"`
	Thumb  string `json:"thumb,omitempty"` // a smaller version for the picker, if the page had one
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Name   string `json:"name,omitempty"`
	Video  bool   `json:"video,omitempty"`

	measured bool // Width and Height are the real size, not declared or unknown
}

// PhotoRequest asks for photos from an inspected link to be saved.
type PhotoRequest struct {
	URL       string  `json:"url"` // the gallery or page link
	Title     string  `json:"title"`
	Source    string  `json:"source"`    // "gallery-dl", "page" or "browser", from inspection
	Total     int     `json:"total"`     // photos found
	Truncated bool    `json:"truncated"` // more exist than were listed
	Items     []Photo `json:"items"`     // the chosen ones
}

// needsCookies reports whether the job uses browser cookies: everything but
// the plain page scan.
func (r *PhotoRequest) needsCookies() bool { return r == nil || r.Source != "page" }

// all reports whether gallery-dl should run without a range: every photo was
// chosen from a listing cut off at inspectLimit, so the rest are wanted too.
// Otherwise the range is used even for "all", since the listing leaves out
// icons and duplicates that gallery-dl would otherwise also save.
func (r *PhotoRequest) all() bool { return r.Truncated && len(r.Items) >= r.Total }

// inspectLimit caps how many files gallery-dl lists for the picker; a
// profile can have thousands. Downloading "all" still gets everything.
const inspectLimit = 1000

var errNoPhotos = errors.New("no video or photos found at this link")

// DownloadPhotos queues one job that saves the chosen photos into a folder
// named after the gallery.
func (e *Engine) DownloadPhotos(req PhotoRequest) Job {
	req.Title = firstNonEmpty(strings.TrimSpace(req.Title), "Photos")
	sort.Slice(req.Items, func(a, b int) bool { return req.Items[a].Index < req.Items[b].Index })
	total := len(req.Items)
	if req.all() && req.Truncated {
		total = 0 // unknown until gallery-dl has gone through them all
	}
	e.mu.Lock()
	e.seq++
	j := &Job{
		ID:        strconv.Itoa(e.seq),
		URL:       req.URL,
		Title:     req.Title,
		State:     Queued,
		Created:   time.Now(),
		Kind:      "photos",
		FileTotal: total,
		photos:    &req,
	}
	if len(req.Items) > 0 {
		j.Thumbnail = firstNonEmpty(req.Items[0].Thumb, req.Items[0].URL)
		if req.Items[0].Video {
			j.Thumbnail = ""
		}
	}
	e.mu.Unlock()
	return e.start(j)
}

// inspectPhotos asks gallery-dl, then (if scan) looks through the page itself.
func (e *Engine) inspectPhotos(ctx context.Context, paths tools.Paths, s config.Settings, url string, scan bool) (Metadata, error) {
	var gdlErr error
	if paths.GalleryDL != "" {
		md, err := e.inspectGalleryDL(ctx, paths, s, url)
		if err == nil && len(md.Photos) > 0 {
			return md, nil
		}
		if ctx.Err() != nil {
			return Metadata{}, ctx.Err()
		}
		if err != nil && !errors.Is(err, errUnsupported) {
			gdlErr = err
		}
	}
	if !scan {
		return Metadata{}, firstErr(gdlErr, errNoPhotos)
	}
	md, err := scanPage(ctx, url)
	if e.worthRendering(ctx, md, err) {
		r, rerr := e.renderPhotos(ctx, paths, s, url)
		switch {
		case rerr != nil:
			slog.Info("crow's nest failed", "url", url, "err", rerr)
		case len(r.Photos) > len(md.Photos), len(r.Photos) == len(md.Photos) && len(r.Links) > len(md.Links), err != nil && !r.empty():
			md, err = r, nil
		default: // the page scan's photos, with the streams only the browser saw
			md.Streams = r.Streams
		}
	}
	if err == nil && !md.empty() {
		return withKind(md), nil
	}
	if gdlErr != nil { // gallery-dl knows the site, so its error says more
		return Metadata{}, gdlErr
	}
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{}, errNoPhotos
}

// withKind marks a result by what it mostly offers: streams, unless there
// are more than a few photos (on a video page those are its poster and the
// like); photos; or else linked videos.
func withKind(md Metadata) Metadata {
	switch {
	case len(md.Streams) > 0 && len(md.Photos) < 3:
		md.Kind = "streams"
	case len(md.Photos) > 0:
	case len(md.Streams) > 0:
		md.Kind = "streams"
	case len(md.Links) > 0:
		md.Kind = "links"
	}
	return md
}

// empty reports whether a page look found nothing to offer.
func (md Metadata) empty() bool {
	return len(md.Photos) == 0 && len(md.Links) == 0 && len(md.Streams) == 0
}

// worthRendering reports whether to try the crow's nest after a page scan:
// it found next to nothing (often just the og:image) or a player, whose
// stream only the browser can catch, or the site turned
// away a plain request, as bot walls do. Only automatically when a browser
// is at hand; downloading headless Chrome waits until it's asked for.
func (e *Engine) worthRendering(ctx context.Context, md Metadata, err error) bool {
	if ctx.Err() != nil || md.direct || !e.tools.BrowserReady() {
		return false
	}
	var status statusError
	if errors.As(err, &status) {
		return status.code == 401 || status.code == 403 || status.code == 429 || status.code == 503
	}
	return err == nil && (len(md.Photos) < 3 || md.player)
}

var errUnsupported = errors.New("unsupported site")

func (e *Engine) inspectGalleryDL(ctx context.Context, paths tools.Paths, s config.Settings, url string) (Metadata, error) {
	cookies, done := e.cookieFile(ctx, s)
	defer done()
	args := append(galleryArgs(s, cookies), "--resolve-json", "--range", "1-"+strconv.Itoa(inspectLimit), "--", url)
	release := e.tools.Use()
	cmd := tools.Command(ctx, paths.GalleryDL, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	release()
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 64 {
		return Metadata{}, errUnsupported
	}
	if err != nil && len(out) == 0 {
		return Metadata{}, galleryError(stderr.String(), err)
	}
	md, perr := parseGalleryJSON(out)
	if perr != nil {
		return Metadata{}, perr
	}
	md.URL = url
	return md, nil
}

// galleryArgs apply to both inspection and download; cookies is as for commonArgs.
func galleryArgs(s config.Settings, cookies string) []string {
	return cookieArgs(s, cookies)
}

// parseGalleryJSON reads gallery-dl's --resolve-json output: a list of
// messages [2, dirinfo], [3, url, fileinfo] and [-1, error].
func parseGalleryJSON(out []byte) (Metadata, error) {
	var msgs [][]json.RawMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		return Metadata{}, fmt.Errorf("parsing gallery-dl output: %w", err)
	}
	md := Metadata{Kind: "photos", Source: "gallery-dl"}
	var info map[string]any // first directory (or file) info, for the title
	var errMsg string
	position := 0 // gallery-dl's own file numbering, which --range refers to
	seen := map[string]bool{}
	for _, m := range msgs {
		if len(m) < 2 {
			continue
		}
		var kind int
		if json.Unmarshal(m[0], &kind) != nil {
			continue
		}
		switch {
		case kind == 2 && info == nil:
			_ = json.Unmarshal(m[1], &info)
		case kind == 3 && len(m) >= 3:
			var link string
			var kw map[string]any
			if json.Unmarshal(m[1], &link) != nil {
				continue
			}
			position++
			_ = json.Unmarshal(m[2], &kw)
			if info == nil {
				info = kw
			}
			w, h := num(kw["width"]), num(kw["height"])
			if !strings.HasPrefix(link, "http") || seen[link] || (w > 0 && h > 0 && w < minPhotoSide && h < minPhotoSide) {
				continue // e.g. a wiki article's interface icons
			}
			seen[link] = true
			ext := str(kw["extension"])
			p := Photo{
				Index:  position,
				URL:    link,
				Width:  w,
				Height: h,
				Video:  isVideoExt(ext),
			}
			if name := str(kw["filename"]); name != "" {
				p.Name = name + "." + ext
			}
			md.Photos = append(md.Photos, p)
		case kind == -1:
			var e map[string]any
			if json.Unmarshal(m[1], &e) == nil {
				errMsg = firstNonEmpty(str(e["message"]), str(e["error"]))
			}
		}
	}
	if len(md.Photos) == 0 {
		if errMsg != "" {
			return Metadata{}, errors.New(errMsg)
		}
		return md, nil
	}
	md.Title, md.Uploader = galleryTitle(info)
	md.Truncated = position >= inspectLimit
	if !md.Photos[0].Video {
		md.Thumbnail = md.Photos[0].URL
	}
	return md, nil
}

// galleryTitle picks a title and owner from gallery-dl's metadata, whose
// keys differ per site.
func galleryTitle(info map[string]any) (title, owner string) {
	for _, k := range []string{"user", "author", "owner", "artist", "blog"} {
		if m, ok := info[k].(map[string]any); ok {
			owner = firstNonEmpty(str(m["name"]), str(m["username"]), str(m["nick"]))
		} else {
			owner = str(info[k])
		}
		if owner != "" {
			break
		}
	}
	owner = firstNonEmpty(owner, str(info["username"]), str(info["uploader"]))
	for _, k := range []string{"album", "gallery", "collection", "set", "board", "post"} {
		if m, ok := info[k].(map[string]any); ok {
			if title = str(m["title"]); title != "" {
				break
			}
		}
	}
	if str(info["category"]) == "directlink" { // a plain image link
		title = str(info["filename"])
	}
	if title == "" {
		if page := str(info["page"]); page != "" { // wikis: "Category:Lighthouses_in_Maine"
			title = strings.ReplaceAll(page, "_", " ")
		}
	}
	title = firstNonEmpty(title, str(info["title"]))
	if title == "" {
		site := str(info["category"])
		if site != "" {
			site = strings.ToUpper(site[:1]) + site[1:]
		}
		title = strings.TrimSpace(strings.Join(nonEmpty(site, owner), " – "))
	}
	return firstNonEmpty(title, "Photos"), owner
}

// runGalleryDL lets gallery-dl download the chosen files into the job's
// folder. It prints each saved path (or "# path" if it already had it).
func (e *Engine) runGalleryDL(ctx context.Context, j *Job, paths tools.Paths, s config.Settings) error {
	if paths.GalleryDL == "" {
		return i18n.New("error.gallery_dl_missing")
	}
	dir := photoDir(s, j)
	cookies, done := e.cookieFile(ctx, s)
	defer done()
	args := append(galleryArgs(s, cookies), "-D", dir)
	if !j.photos.all() {
		args = append(args, "--range", rangeSpec(j.photos.Items))
	}
	args = append(args, "--", j.URL)
	slog.Info("photos starting", "url", j.URL, "source", "gallery-dl", "files", j.FileTotal)
	e.mutate(j, func(j *Job) { j.State, j.FilePath = Downloading, dir })

	release := e.tools.Use()
	defer release()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd := tools.Command(ctx, paths.GalleryDL, args...)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	errText := make(chan string, 1)
	readDone := make(chan struct{})
	go func() { errText <- collectErrors(errR, galleryErrorLine) }()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			e.mutate(j, func(j *Job) {
				j.Files++
				if j.FileTotal > 0 {
					j.Percent = float64(j.Files) * 100 / float64(j.FileTotal)
				}
			})
		}
		_, _ = io.Copy(io.Discard, outR)
		close(readDone)
	}()
	waitErr := cmd.Wait()
	outW.Close()
	errW.Close()
	<-readDone
	stderrText := <-errText
	if waitErr != nil {
		err := galleryError(stderrText, waitErr)
		if j.Files > 0 {
			return i18n.New("error.photos_partial", "count", strconv.Itoa(j.Files)).WithDetail(err.Error())
		}
		return err
	}
	if j.Files == 0 {
		return errNoPhotos
	}
	return nil
}

// photoDir is the gallery's own folder, or the download folder itself for a single photo.
func photoDir(s config.Settings, j *Job) string {
	if j.photos.Total == 1 {
		return s.OutputDir
	}
	return filepath.Join(s.OutputDir, sanitizeDirName(j.Title))
}

// galleryErrorLine picks out lines like "[example][error] HttpError: ...".
func galleryErrorLine(line string) (string, bool) {
	i := strings.Index(line, "][error] ")
	if i < 0 {
		return "", false
	}
	return line[i+len("][error] "):], true
}

func galleryError(stderr string, err error) error {
	if msg := strings.TrimSpace(collectErrors(strings.NewReader(stderr), galleryErrorLine)); msg != "" {
		return errors.New(msg)
	}
	return err
}

// rangeSpec turns chosen photos into gallery-dl's --range, e.g. "1-3,7,9-10".
func rangeSpec(items []Photo) string {
	var parts []string
	for i := 0; i < len(items); {
		start, end := items[i].Index, items[i].Index
		for i++; i < len(items) && items[i].Index == end+1; i++ {
			end++
		}
		if start == end {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, end))
		}
	}
	return strings.Join(parts, ",")
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func isVideoExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "mp4", "webm", "mov", "m4v", "mkv", "gifv":
		return true
	}
	return false
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
