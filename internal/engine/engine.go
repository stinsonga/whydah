// Package engine inspects links and runs downloads: video/audio via yt-dlp,
// photos via gallery-dl or a built-in page scanner. Jobs share one queue with
// concurrency limiting, cancellation and progress reporting via events.
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"whydah/internal/config"
	"whydah/internal/events"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

type State string

const (
	Queued      State = "queued"
	Downloading State = "downloading"
	Processing  State = "processing" // merging / converting after a stream finished
	Done        State = "done"
	Failed      State = "error"
	Canceled    State = "canceled"
)

func (s State) Finished() bool { return s == Done || s == Failed || s == Canceled }

type Job struct {
	ID         string      `json:"id"`
	URL        string      `json:"url"`
	Title      string      `json:"title"`
	Problem    *i18n.Error `json:"problem,omitempty"` // why it failed, explained; Error is the same in English
	Thumbnail  string      `json:"thumbnail"`
	Audio      bool        `json:"audio"`
	State      State       `json:"state"`
	Part       int         `json:"part"` // 1-based stream index (video and audio download separately)
	Percent    float64     `json:"percent"`
	Downloaded int64       `json:"downloaded"`
	Total      int64       `json:"total"`
	Speed      float64     `json:"speed"`
	ETA        int         `json:"eta"`
	FilePath   string      `json:"file_path"`
	Error      string      `json:"error"`
	Created    time.Time   `json:"created"`

	// Set for items of a playlist download; jobs sharing Group are shown together.
	Group      string `json:"group,omitempty"`
	GroupTitle string `json:"group_title,omitempty"`
	Index      int    `json:"index,omitempty"` // 1-based position in the playlist
	GroupSize  int    `json:"group_size,omitempty"`
	pad        int

	// A photo job saves a whole gallery (or the chosen photos) into one folder;
	// FilePath is that folder and Files counts photos saved so far.
	Kind      string `json:"kind,omitempty"` // "photos", or "" for video/audio
	Files     int    `json:"files,omitempty"`
	FileTotal int    `json:"file_total,omitempty"` // 0 if unknown (a gallery too big to list)
	photos    *PhotoRequest

	// A stream the crow's nest caught is fetched the way the page's player
	// fetched it, and saved under the page's title (the stream's own is
	// usually "master" or "index").
	Referer string `json:"referer,omitempty"`
	Origin  string `json:"origin,omitempty"`
	Page    string `json:"page,omitempty"` // the page the stream was found on
}

// Request describes one download. The Group fields are set by DownloadPlaylist.
type Request struct {
	URL       string
	Audio     bool
	Title     string
	Thumbnail string
	Referer   string // set for a stream: see Job.Referer
	Origin    string
	Page      string

	group, groupTitle string
	index, groupSize  int
	pad               int // digits in the filename's track number
}

type Metadata struct {
	URL            string  `json:"url"`
	Title          string  `json:"title"`
	Uploader       string  `json:"uploader"`
	Duration       float64 `json:"duration"`
	DurationString string  `json:"duration_string"`
	Thumbnail      string  `json:"thumbnail"`
	FilesizeApprox int64   `json:"filesize_approx"`

	Kind        string  `json:"kind"`         // "video", "playlist", "photos", "streams" (only those) or "links" (linked videos only)
	Entries     []Entry `json:"entries"`      // playlist items, in order
	PlaylistURL string  `json:"playlist_url"` // for a video opened from a playlist (a watch link with &list=)

	Photos    []Photo `json:"photos"`    // for "photos", in order
	Source    string  `json:"source"`    // for "photos": "gallery-dl", "page" or "browser"; "links" for a playlist of linked videos
	Truncated bool    `json:"truncated"` // more photos exist than were listed

	Links []LinkedVideo `json:"links"` // thumbnails that seem to lead to videos (Kind "photos" or "links")

	Streams []Stream `json:"streams"` // streams the crow's nest saw the page play (any Kind but "video" and "playlist")

	direct bool // the link is an image itself, not a page
	player bool // the page scan saw signs of a video or audio player
}

type Engine struct {
	settings *config.Store
	tools    *tools.Manager
	hub      *events.Hub

	mu      sync.Mutex
	slots   *sync.Cond // signalled when a running job finishes or a job is canceled
	running int
	seq     int
	wg      sync.WaitGroup // one per run goroutine, so Shutdown can wait for subprocesses
	jobs    map[string]*Job
	cancels map[string]context.CancelFunc

	browsers      browserPool // the crow's nest's browser, and the sign-in window's
	signInCookies signInCookies
	history       history

	// The saved queue (see queue.go); queuePath is "" when it isn't kept.
	queuePath string
	closing   bool // shutting down: jobs canceled now are saved as still to do
	saveReq   chan struct{}
	saveMu    sync.Mutex
}

func New(settings *config.Store, tm *tools.Manager, hub *events.Hub) *Engine {
	e := &Engine{
		settings: settings,
		tools:    tm,
		hub:      hub,
		jobs:     make(map[string]*Job),
		cancels:  make(map[string]context.CancelFunc),
		saveReq:  make(chan struct{}, 1),
	}
	e.slots = sync.NewCond(&e.mu)
	tm.BeforeRefit(e.browsers.closeIdle) // a refit may replace the browser; no look runs during one
	return e
}

// InspectOptions narrow what Inspect looks for.
type InspectOptions struct {
	PhotosOnly bool `json:"photos"`     // skip the video lookup, for pages that have both
	CrowsNest  bool `json:"crows_nest"` // go straight to the hidden browser, for script-built pages
}

// Inspect finds what a link offers without downloading: a video or playlist
// (yt-dlp), or else photos (gallery-dl, then scanning the page itself, then
// if that finds little, the crow's nest). Its error is explained (an
// *i18n.Error), unless the look was canceled.
func (e *Engine) Inspect(ctx context.Context, url string, opt InspectOptions) (Metadata, error) {
	md, err := e.inspect(ctx, url, opt)
	return md, explainErr(err, url, e.settings.Get())
}

func (e *Engine) inspect(ctx context.Context, url string, opt InspectOptions) (Metadata, error) {
	paths, err := e.tools.Wait(ctx)
	if err != nil {
		return Metadata{}, err
	}
	s := e.settings.Get()
	if err := config.CookieAccess(s.BrowserCookies); err != nil {
		return Metadata{}, err
	}
	if opt.CrowsNest {
		md, err := e.renderPhotos(ctx, paths, s, url)
		if err == nil && md.empty() {
			err = errNoPhotos
		}
		return withKind(md), err
	}
	var videoErr error
	if !opt.PhotosOnly {
		md, err := e.inspectVideo(ctx, paths, s, url)
		if err == nil || ctx.Err() != nil {
			return md, err
		}
		videoErr = err
	}
	// A real error from yt-dlp (e.g. sign-in needed) means it's a video page:
	// a page scan would only find its preview image, so let the error through.
	scan := videoErr == nil || notAVideo(videoErr)
	md, err := e.inspectPhotos(ctx, paths, s, url, scan)
	if err == nil || ctx.Err() != nil {
		return md, err
	}
	// "Unsupported URL" from yt-dlp just means it's not a video page; anything
	// else (private, needs sign-in, ...) is more useful than "no photos found".
	if videoErr != nil && !notAVideo(videoErr) {
		err = videoErr
	}
	e.tools.CheckSoon() // a broken site is often fixed by a newer release
	return Metadata{}, err
}

func (e *Engine) inspectVideo(ctx context.Context, paths tools.Paths, s config.Settings, url string) (Metadata, error) {
	cookies, done := e.cookieFile(ctx, s)
	defer done()
	args := append(commonArgs(paths, s, cookies), "-J", "--no-playlist", "--flat-playlist", url)
	release := e.tools.Use()
	cmd := tools.Command(ctx, paths.YtDlp, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	release()
	if err != nil {
		return Metadata{}, ytError(stderr.String(), err)
	}
	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return Metadata{}, fmt.Errorf("parsing yt-dlp output: %w", err)
	}
	if imageExt("."+info.Ext) != "" {
		// yt-dlp's generic extractor accepts a direct image link as a "video".
		return Metadata{}, errors.New("Unsupported URL: an image, not a video")
	}
	md := Metadata{
		URL:            url,
		Title:          info.Title,
		Uploader:       firstNonEmpty(info.Uploader, info.Channel),
		Duration:       info.Duration,
		DurationString: formatDuration(info.Duration),
		Thumbnail:      info.thumbnail(),
		FilesizeApprox: max(info.FilesizeApprox, info.Filesize),
		Kind:           "video",
	}
	if info.Type == "playlist" {
		md.Kind = "playlist"
		md.Entries = playlistEntries(info.Entries)
		if len(md.Entries) == 0 {
			return Metadata{}, i18n.New("error.playlist_empty")
		}
		if md.Thumbnail == "" {
			md.Thumbnail = md.Entries[0].Thumbnail
		}
	} else {
		md.PlaylistURL = watchPlaylistURL(url)
	}
	return md, nil
}

// Download queues a download and returns immediately.
func (e *Engine) Download(req Request) Job {
	e.mu.Lock()
	e.seq++
	j := &Job{
		ID:         strconv.Itoa(e.seq),
		URL:        req.URL,
		Title:      firstNonEmpty(req.Title, req.URL),
		Thumbnail:  req.Thumbnail,
		Audio:      req.Audio,
		Referer:    req.Referer,
		Origin:     req.Origin,
		Page:       req.Page,
		State:      Queued,
		Created:    time.Now(),
		Group:      req.group,
		GroupTitle: req.groupTitle,
		Index:      req.index,
		GroupSize:  req.groupSize,
		pad:        req.pad,
	}
	e.mu.Unlock()
	return e.start(j)
}

// start registers a new job and runs it in the background.
func (e *Engine) start(j *Job) Job {
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.jobs[j.ID] = j
	e.cancels[j.ID] = cancel
	snap := *j
	e.wg.Add(1)
	e.mu.Unlock()

	e.hub.Publish(events.JobUpdated, snap)
	e.queueChanged()
	go e.run(ctx, j)
	return snap
}

func (e *Engine) Cancel(id string) bool {
	e.mu.Lock()
	cancel, ok := e.cancels[id]
	e.mu.Unlock()
	if ok {
		cancel()
		e.wake() // in case it's still waiting for a slot
	}
	return ok
}

// wake re-checks slot availability for queued jobs, e.g. after MaxConcurrent changed.
// Broadcasting under the lock means a waiter can't miss it between its check and Wait.
func (e *Engine) wake() {
	e.mu.Lock()
	e.slots.Broadcast()
	e.mu.Unlock()
}

// SettingsChanged lets queued jobs start if the concurrency limit went up.
func (e *Engine) SettingsChanged() { e.wake() }

// Jobs returns a snapshot of all jobs, oldest first.
func (e *Engine) Jobs() []Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Job, 0, len(e.jobs))
	for _, j := range e.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out
}

func (e *Engine) Job(id string) (Job, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// ClearFinished forgets done/failed/canceled jobs.
func (e *Engine) ClearFinished() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, j := range e.jobs {
		if j.State.Finished() {
			delete(e.jobs, id)
		}
	}
	e.queueChanged()
}

// CancelAll stops every job.
func (e *Engine) CancelAll() {
	e.mu.Lock()
	for _, c := range e.cancels {
		c()
	}
	e.slots.Broadcast()
	e.mu.Unlock()
}

// Shutdown cancels every job and waits (up to timeout) for their processes
// to be killed, so nothing keeps downloading after the app exits.
func (e *Engine) Shutdown(timeout time.Duration) {
	// Save before canceling, so running downloads are kept as still to do.
	e.saveQueue()
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	e.CancelAll()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("shutdown timed out waiting for downloads to stop")
	}
	e.browsers.close()
}

// mutate applies f to the job under the lock and publishes the result.
func (e *Engine) mutate(j *Job, f func(*Job)) {
	e.mu.Lock()
	before := *j
	f(j)
	snap := *j
	e.mu.Unlock()
	e.hub.Publish(events.JobUpdated, snap)
	// Progress alone isn't worth a save; a resumed job starts its count again.
	if snap.State != before.State || snap.FilePath != before.FilePath || snap.Title != before.Title || snap.Thumbnail != before.Thumbnail {
		e.queueChanged()
	}
}

func (e *Engine) acquire(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for e.running >= e.settings.Get().MaxConcurrent && ctx.Err() == nil {
		e.slots.Wait()
	}
	if ctx.Err() != nil {
		return false
	}
	e.running++
	return true
}

func (e *Engine) release() {
	e.mu.Lock()
	e.running--
	e.mu.Unlock()
	e.slots.Broadcast()
}

func (e *Engine) run(ctx context.Context, j *Job) {
	defer e.wg.Done()
	// finish records the final state and drops the cancel func in one step, so
	// a Retry can never see a finished job that still looks cancelable.
	finish := func(state State, msg string) {
		e.mutate(j, func(j *Job) {
			delete(e.cancels, j.ID)
			j.State, j.Error, j.Speed, j.ETA = state, msg, 0, 0
			if state == Done {
				j.Percent = 100
			}
		})
	}
	fail := func(err error) {
		slog.Warn("download ended", "url", j.URL, "canceled", ctx.Err() != nil, "err", err)
		if ctx.Err() != nil {
			finish(Canceled, "")
			return
		}
		e.tools.CheckSoon() // a broken site is often fixed by a newer yt-dlp
		problem := i18n.From(explainErr(err, j.URL, e.settings.Get()))
		e.mutate(j, func(j *Job) { j.Problem = problem })
		finish(Failed, problem.Error())
	}

	paths, err := e.tools.Wait(ctx)
	if err != nil {
		fail(err)
		return
	}
	if !e.acquire(ctx) {
		fail(ctx.Err())
		return
	}
	defer e.release()

	s := e.settings.Get()
	if err := config.CookieAccess(s.BrowserCookies); err != nil && j.photos.needsCookies() {
		fail(err)
		return
	}
	if err := os.MkdirAll(s.OutputDir, 0o755); err != nil {
		fail(i18n.New("error.output_folder", "path", s.OutputDir).WithDetail(err.Error()))
		return
	}
	switch {
	case j.photos != nil && j.photos.Source == "gallery-dl":
		err = e.runGalleryDL(ctx, j, paths, s)
	case j.photos != nil:
		err = e.runPagePhotos(ctx, j, paths, s)
	default:
		err = e.runYtDlp(ctx, j, paths, s)
	}
	if err != nil {
		fail(err)
		return
	}
	slog.Info("download done", "url", j.URL, "file", j.FilePath)
	finish(Done, "")
	e.mu.Lock()
	snap := *j
	e.mu.Unlock()
	e.history.add(snap)
}

func (e *Engine) runYtDlp(ctx context.Context, j *Job, paths tools.Paths, s config.Settings) error {
	cookies, done := e.cookieFile(ctx, s)
	defer done()
	args := append(commonArgs(paths, s, cookies), downloadArgs(paths, s, j)...)
	args = append(args, "--", j.URL)
	slog.Info("download starting", "url", j.URL, "audio", j.Audio)
	e.mutate(j, func(j *Job) { j.State = Downloading })

	// io.Pipe + WaitDelay (rather than StdoutPipe) so Wait can't hang if a
	// killed yt-dlp leaves an ffmpeg child holding the output open.
	release := e.tools.Use() // an update waits for this download to finish
	defer release()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	cmd := tools.Command(ctx, paths.YtDlp, args...)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}

	errText := make(chan string, 1)
	progressDone := make(chan struct{})
	go func() { errText <- collectErrors(errR, ytErrorLine) }()
	go func() { e.readProgress(j, outR); close(progressDone) }()
	waitErr := cmd.Wait()
	outW.Close()
	errW.Close()
	<-progressDone
	stderrText := <-errText
	if waitErr != nil {
		return ytError(stderrText, waitErr)
	}
	return nil
}

type progressLine struct {
	Status         string   `json:"status"`
	Filename       string   `json:"filename"`
	Downloaded     int64    `json:"downloaded_bytes"`
	Total          *int64   `json:"total_bytes"`
	TotalEstimate  *float64 `json:"total_bytes_estimate"`
	Speed          *float64 `json:"speed"`
	ETA            *float64 `json:"eta"`
	PercentDecimal *float64 `json:"_percent"`
}

func (e *Engine) readProgress(j *Job, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lastFile string
	var lastPublish time.Time
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "FINAL:"):
			path := strings.TrimPrefix(line, "FINAL:")
			e.mutate(j, func(j *Job) { j.FilePath = path })
		case strings.HasPrefix(line, "TITLE:"):
			// Some playlists only give links until each item resolves.
			if title := strings.TrimPrefix(line, "TITLE:"); title != "" && title != "NA" {
				e.mutate(j, func(j *Job) { j.Title = title })
			}
		case strings.HasPrefix(line, "THUMB:"):
			if thumb := strings.TrimPrefix(line, "THUMB:"); strings.HasPrefix(thumb, "http") {
				e.mutate(j, func(j *Job) {
					if j.Thumbnail == "" {
						j.Thumbnail = thumb
					}
				})
			}
		case strings.HasPrefix(line, "PROG:"):
			var p progressLine
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "PROG:")), &p) != nil {
				continue
			}
			newPart := p.Filename != lastFile
			lastFile = p.Filename
			// Throttle the chatty "downloading" lines; always publish transitions.
			if p.Status == "downloading" && !newPart && time.Since(lastPublish) < 250*time.Millisecond {
				continue
			}
			lastPublish = time.Now()
			e.mutate(j, func(j *Job) {
				if newPart {
					j.Part++
				}
				j.Downloaded = p.Downloaded
				switch {
				case p.Total != nil:
					j.Total = *p.Total
				case p.TotalEstimate != nil:
					j.Total = int64(*p.TotalEstimate)
				}
				if p.PercentDecimal != nil {
					j.Percent = *p.PercentDecimal
				} else if j.Total > 0 {
					j.Percent = float64(j.Downloaded) * 100 / float64(j.Total)
				}
				j.Speed, j.ETA = 0, 0
				if p.Speed != nil {
					j.Speed = *p.Speed
				}
				if p.ETA != nil {
					j.ETA = int(*p.ETA)
				}
				if p.Status == "finished" {
					j.State, j.Speed, j.ETA = Processing, 0, 0
				} else {
					j.State = Downloading
				}
			})
		}
	}
	_, _ = io.Copy(io.Discard, r) // keep draining if the scanner gave up on a huge line
}

// commonArgs apply to both inspection and download. cookies is a
// cookies.txt to use instead of the browser's, or "" (see cookieFile).
func commonArgs(p tools.Paths, s config.Settings, cookies string) []string {
	args := []string{"--no-colors", "--no-warnings"}
	if p.Deno != "" {
		args = append(args, "--js-runtimes", "deno:"+p.Deno)
	}
	if p.FFmpegDir != "" {
		args = append(args, "--ffmpeg-location", p.FFmpegDir)
	}
	args = append(args, cookieArgs(s, cookies)...)
	return args
}

// cookieArgs tell yt-dlp or gallery-dl (they take the same options) which cookies to use.
func cookieArgs(s config.Settings, cookies string) []string {
	switch {
	case cookies != "":
		return []string{"--cookies", cookies}
	case s.BrowserCookies != "" && s.BrowserCookies != config.OwnBrowser:
		return []string{"--cookies-from-browser", s.BrowserCookies}
	}
	return nil
}

func downloadArgs(p tools.Paths, s config.Settings, j *Job) []string {
	audio := j.Audio
	args := []string{
		"--no-playlist", "--newline", "--progress",
		"--progress-template", "download:PROG:%(progress)j",
		"--print", "video:TITLE:%(title)s",
		"--print", "video:THUMB:%(thumbnail)s",
		"--print", "after_move:FINAL:%(filepath)s",
		"-o", outputTemplate(s.OutputDir, s.NameTemplate, j),
		"--embed-metadata",
	}
	args = append(args, playlistMetadataArgs(j)...)
	if j.Referer != "" {
		args = append(args, streamArgs(j.Referer, j.Origin)...)
		args = append(args, "--parse-metadata", literalMetadata(j.Title)+":%(title)s")
	}
	hasFFmpeg := p.FFmpegDir != ""
	switch {
	case audio && hasFFmpeg:
		args = append(args, "-f", "ba/b", "-x", "--audio-format", s.AudioFormat, "--audio-quality", "0")
		if s.AudioFormat != "wav" { // wav has no cover art support
			args = append(args, "--embed-thumbnail")
		}
	case audio:
		args = append(args, "-f", "ba[ext=m4a]/ba/b")
	case hasFFmpeg:
		args = append(args, "-f", "bv*+ba/b")
		var sortKeys []string
		if s.VideoQuality != "best" {
			sortKeys = append(sortKeys, "res:"+s.VideoQuality)
		}
		if s.CompatibleMP4 {
			sortKeys = append(sortKeys, "vcodec:h264", "ext:mp4:m4a")
			args = append(args, "--merge-output-format", "mp4")
		}
		if len(sortKeys) > 0 {
			args = append(args, "-S", strings.Join(sortKeys, ","))
		}
	default:
		// No ffmpeg means no merging: take the best single file that has both tracks.
		f := "b"
		if s.VideoQuality != "best" {
			f = "b[height<=" + s.VideoQuality + "]/b"
		}
		args = append(args, "-f", f)
	}
	return args
}

// collectErrors keeps the error lines picked out by isErr (or the tail of
// stderr if there are none).
func collectErrors(r io.Reader, isErr func(string) (string, bool)) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var errs, tail []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if msg, ok := isErr(line); ok {
			errs = append(errs, msg)
		}
		tail = append(tail, line)
		if len(tail) > 5 {
			tail = tail[1:]
		}
	}
	_, _ = io.Copy(io.Discard, r)
	if len(errs) > 0 {
		return strings.Join(errs, "\n")
	}
	return strings.Join(tail, "\n")
}

func ytErrorLine(line string) (string, bool) { return line, strings.HasPrefix(line, "ERROR:") }

// notAVideo reports whether a yt-dlp error just means the page has no video,
// as opposed to a video that couldn't be fetched.
func notAVideo(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.HasPrefix(msg, "[generic]") { // no extractor knows the site, so whatever went wrong isn't about a video
		return true
	}
	for _, s := range []string{"unsupported url", "no video", "no media", "not a video", "no formats", "there is no video"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func ytError(stderr string, err error) error {
	var exitErr interface{ ExitCode() int }
	if msg := strings.TrimSpace(stderr); msg != "" && errors.As(err, &exitErr) {
		if !strings.Contains(msg, "\n") {
			msg = strings.TrimPrefix(msg, "ERROR: ")
		}
		return errors.New(msg)
	}
	return err
}

func formatDuration(sec float64) string {
	if sec <= 0 {
		return "Unknown"
	}
	s := int(sec + 0.5)
	h, m, s := s/3600, s%3600/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
