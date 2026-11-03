// Package cli is the terminal mode: salvage (download), spyglass (look
// without downloading), refit (update the tools) and sign-in (open the
// sign-in browser).
package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"whydah/internal/engine"
	"whydah/internal/events"
	"whydah/internal/i18n"
	"whydah/internal/selfupdate"
	"whydah/internal/tools"
)

type printer struct {
	pending   map[string]bool
	lastPrint map[string]time.Time
	lastState map[string]engine.State
	lastStage string // in Lang
	failed    int
}

func (p *printer) handle(ev events.Event) {
	switch d := ev.Data.(type) {
	case tools.Status:
		if d.Stage != nil && d.Stage.In(Lang) != p.lastStage {
			p.lastStage = d.Stage.In(Lang)
			fmt.Println("·", p.lastStage)
		}
	case engine.Job:
		p.job(d)
	}
}

func (p *printer) job(j engine.Job) {
	if !p.pending[j.ID] {
		return
	}
	changed := p.lastState[j.ID] != j.State
	p.lastState[j.ID] = j.State
	if !changed && time.Since(p.lastPrint[j.ID]) < 2*time.Second {
		return
	}
	p.lastPrint[j.ID] = time.Now()
	fmt.Println(describe(j))
	if j.State.Finished() {
		delete(p.pending, j.ID)
		if j.State != engine.Done {
			p.failed++
		}
	}
}

// waitForTools prints setup progress until the tools are ready, returning
// a non-zero exit code if setup failed or was interrupted.
func waitForTools(ctx context.Context, tm *tools.Manager, ch <-chan events.Event, p *printer) int {
	ready := make(chan error, 1)
	go func() { _, err := tm.Wait(ctx); ready <- err }()
	for waiting := true; waiting; {
		select {
		case err := <-ready:
			if err != nil {
				fmt.Println("✗", Explained(err))
				return 1
			}
			waiting = false
		case ev := <-ch:
			p.handle(ev)
		case <-ctx.Done():
			return 130
		}
	}
	for _, w := range tm.Status().Warnings {
		fmt.Println("!", problemText(w))
	}
	return 0
}

func newPrinter() *printer {
	return &printer{pending: map[string]bool{}, lastPrint: map[string]time.Time{}, lastState: map[string]engine.State{}}
}

// Salvage downloads everything at each URL and returns a process exit code.
// With linked, a page's linked videos are checked and salvaged instead of its photos.
func Salvage(ctx context.Context, eng *engine.Engine, tm *tools.Manager, hub *events.Hub, urls []string, audio, linked bool, opt engine.InspectOptions) int {
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	p := newPrinter()
	if code := waitForTools(ctx, tm, ch, p); code != 0 {
		return code
	}

	for _, url := range urls {
		md, err := eng.Inspect(ctx, url, opt)
		if err != nil {
			if ctx.Err() != nil {
				return 130
			}
			fmt.Printf("✗ %s: %s\n", url, Explained(err))
			p.failed++
			continue
		}
		if linked && len(md.Links) > 0 {
			fmt.Println(T("cli.checking_linked", "count", strconv.Itoa(len(md.Links)), "title", md.Title))
			if md, err = eng.LinkedPlaylist(ctx, url, md.Title, md.Links); err != nil {
				if ctx.Err() != nil {
					return 130
				}
				fmt.Printf("✗ %s: %s\n", url, Explained(err))
				p.failed++
				continue
			}
		} else if len(md.Links) > 0 {
			fmt.Println(T("cli.linked_hint", "count", strconv.Itoa(len(md.Links))))
		}
		for _, st := range md.Streams {
			fmt.Println(T("cli.stream", "title", st.Title, "about", streamLine(st)))
			j := eng.Download(engine.Request{URL: st.URL, Audio: audio || st.Kind == "audio", Title: st.Title,
				Thumbnail: st.Thumbnail, Referer: st.Referer, Origin: st.Origin, Page: url})
			p.pending[j.ID] = true
		}
		if md.Kind == "links" || md.Kind == "streams" {
			continue
		}
		if md.Kind == "photos" {
			fmt.Println(T("cli.photos", "title", md.Title, "photos", photoCount(md)))
			j := eng.DownloadPhotos(engine.PhotoRequest{URL: url, Title: md.Title, Source: md.Source,
				Total: len(md.Photos), Truncated: md.Truncated, Items: md.Photos})
			p.pending[j.ID] = true
			continue
		}
		if md.Kind == "playlist" {
			fmt.Println(T("cli.playlist", "title", md.Title, "items", T("ui.items", "count", strconv.Itoa(len(md.Entries)))))
			items := make([]engine.Item, len(md.Entries))
			for i, en := range md.Entries {
				items[i] = engine.Item{Index: en.Index, URL: en.URL, Title: en.Title, Thumbnail: en.Thumbnail, StreamFrom: en.StreamFrom}
			}
			for _, j := range eng.DownloadPlaylist(md.Title, len(md.Entries), audio, items) {
				p.pending[j.ID] = true
			}
			continue
		}
		fmt.Println(T("cli.video", "title", md.Title, "uploader", md.Uploader, "duration", md.DurationString))
		j := eng.Download(engine.Request{URL: url, Audio: audio, Title: md.Title, Thumbnail: md.Thumbnail})
		p.pending[j.ID] = true
	}

	// The hub may drop events if we fell behind while inspecting, so also poll.
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for len(p.pending) > 0 {
		select {
		case <-ctx.Done():
			fmt.Println(T("cli.canceled")) // main's eng.Shutdown stops the downloads
			return 130
		case ev := <-ch:
			p.handle(ev)
		case <-poll.C:
			for id := range p.pending {
				if j, ok := eng.Job(id); ok && j.State.Finished() {
					p.job(j)
				}
			}
		}
	}
	if p.failed > 0 {
		hintUpdate(ctx, tm)
		return 1
	}
	return 0
}

// Spyglass shows what each URL has without downloading anything.
// With linked, a page's linked videos are checked with yt-dlp and listed.
func Spyglass(ctx context.Context, eng *engine.Engine, tm *tools.Manager, hub *events.Hub, urls []string, linked bool, opt engine.InspectOptions) int {
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	if code := waitForTools(ctx, tm, ch, newPrinter()); code != 0 {
		return code
	}
	failed := 0
	for _, url := range urls {
		md, err := eng.Inspect(ctx, url, opt)
		if err != nil {
			if ctx.Err() != nil {
				return 130
			}
			fmt.Printf("✗ %s: %s\n", url, Explained(err))
			failed++
			continue
		}
		fmt.Println(url)
		switch md.Kind {
		case "photos":
			via := map[string]string{"page": T("ui.via_page"), "gallery-dl": T("ui.via_gallery_dl"), "browser": T("ui.via_browser")}[md.Source]
			fmt.Println(T("cli.spy_photos", "title", md.Title, "photos", photoCount(md), "via", via))
			for _, ph := range md.Photos {
				size := ""
				if ph.Width > 0 {
					size = fmt.Sprintf(" %d×%d", ph.Width, ph.Height)
				}
				fmt.Printf("  %4d%s  %s\n", ph.Index, size, ph.URL)
			}
		case "playlist":
			fmt.Println(T("cli.spy_playlist", "title", md.Title, "items", T("ui.items", "count", strconv.Itoa(len(md.Entries)))))
			for _, en := range md.Entries {
				fmt.Printf("  %4d  %s  [%s]\n", en.Index, en.Title, en.DurationString)
			}
		case "links", "streams":
		default:
			fmt.Println(T("cli.spy_video", "title", md.Title, "uploader", md.Uploader, "duration", md.DurationString))
			if md.PlaylistURL != "" {
				fmt.Println(T("cli.spy_from_playlist", "url", md.PlaylistURL))
			}
		}
		for i, st := range md.Streams {
			fmt.Println(T("cli.spy_stream", "n", strconv.Itoa(i+1), "title", st.Title, "about", streamLine(st)))
			fmt.Println("        " + st.URL)
		}
		if len(md.Links) == 0 {
			continue
		}
		if !linked {
			fmt.Println(T("cli.spy_links", "count", strconv.Itoa(len(md.Links))))
			for _, l := range md.Links {
				fmt.Printf("        %s  %s\n", l.Title, l.URL)
			}
			continue
		}
		pl, err := eng.LinkedPlaylist(ctx, url, md.Title, md.Links)
		if err != nil {
			if ctx.Err() != nil {
				return 130
			}
			fmt.Println(T("cli.spy_linked_error", "detail", Explained(err)))
			continue
		}
		fmt.Println(T("cli.spy_linked", "count", strconv.Itoa(len(pl.Entries)), "total", strconv.Itoa(len(md.Links))))
		for _, en := range pl.Entries {
			fmt.Printf("  %4d  %s  [%s]  %s\n", en.Index, en.Title, en.DurationString, en.URL)
		}
	}
	if failed > 0 {
		hintUpdate(ctx, tm)
		return 1
	}
	return 0
}

func photoCount(md engine.Metadata) string {
	n := strconv.Itoa(len(md.Photos))
	if md.Truncated {
		return T("ui.photos_more", "count", n)
	}
	return T("ui.kind_photos", "count", n)
}

// hintUpdate suggests a refit after failures, if newer tools exist.
func hintUpdate(ctx context.Context, tm *tools.Manager) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if tm.Check(cctx) != nil {
		return
	}
	if u := tm.Status().Update; u.Available {
		fmt.Println()
		fmt.Println(T("cli.update_hint", "pending", pendingList(u.Pending)))
	}
}

func pendingList(pending []tools.PendingUpdate) string {
	var parts []string
	for _, p := range pending {
		parts = append(parts, fmt.Sprintf("%s %s → %s", p.Tool, p.Current, p.Latest))
	}
	return strings.Join(parts, ", ")
}

// SignIn opens the sign-in window at link (or its new tab page) and waits
// for it to be closed.
func SignIn(ctx context.Context, eng *engine.Engine, hub *events.Hub, link string) int {
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	if err := eng.SignIn(ctx, link); err != nil {
		fmt.Fprintln(os.Stderr, "✗", Explained(err))
		return 1
	}
	fmt.Println(T("cli.signin_wait"))
	for {
		select {
		case ev := <-ch:
			if st, ok := ev.Data.(engine.SignInStatus); ok && !st.Open {
				fmt.Println(T("cli.signin_done", "option", T("ui.cookies_own")))
				return 0
			}
		case <-ctx.Done():
			return 130
		}
	}
}

// Refit updates yt-dlp and gallery-dl and returns a process exit code.
func Refit(ctx context.Context, tm *tools.Manager) int {
	if _, err := tm.Wait(ctx); err != nil {
		fmt.Println("✗", Explained(err))
		return 1
	}
	before := installed(tm.Status())
	fmt.Println(T("ui.refit_installing"))
	err := tm.Install(ctx)
	after := installed(tm.Status())
	for _, tool := range []string{"yt-dlp", "gallery-dl"} {
		switch {
		case after[tool] == "":
		case after[tool] != before[tool]:
			fmt.Println(T("cli.tool_updated", "tool", tool, "old", before[tool], "new", after[tool]))
		default:
			fmt.Println(T("cli.tool_current", "tool", tool, "version", after[tool]))
		}
	}
	if err != nil {
		fmt.Println("✗", Explained(err))
		return 1
	}
	return 0
}

// Update installs Whydah's latest release over this copy and returns a
// process exit code.
func Update(ctx context.Context, u *selfupdate.Updater) int {
	if !u.Status().Supported {
		fmt.Println(T("cli.update_dev"))
		return 1
	}
	if err := u.Check(ctx); err != nil {
		fmt.Println("✗", Explained(err))
		return 1
	}
	st := u.Status()
	if !st.Available {
		fmt.Println(T("cli.update_current", "version", st.Current))
		return 0
	}
	fmt.Println(T("cli.update_installing", "current", st.Current, "latest", st.Latest))
	if err := u.Install(ctx); err != nil {
		fmt.Println("✗", Explained(err))
		return 1
	}
	fmt.Println(T("cli.update_done", "latest", st.Latest))
	return 0
}

func installed(st tools.Status) map[string]string {
	gdl := st.GalleryDLVer
	if release, build, ok := strings.Cut(gdl, ":"); ok {
		gdl = T("cli.build", "release", release, "build", build)
	}
	return map[string]string{"yt-dlp": st.YtDlpVersion, "gallery-dl": gdl}
}

func describe(j engine.Job) string {
	name := j.Title
	if len([]rune(name)) > 50 {
		name = string([]rune(name)[:49]) + "…"
	}
	if j.Group != "" {
		name = fmt.Sprintf("%d/%d %s", j.Index, j.GroupSize, name)
	}
	if j.Kind == "photos" {
		count := T("ui.kind_photos", "count", strconv.Itoa(j.Files))
		if j.FileTotal > 0 {
			count = T("ui.photos_progress", "count", strconv.Itoa(j.Files), "total", strconv.Itoa(j.FileTotal))
		}
		switch j.State {
		case engine.Queued:
			return fmt.Sprintf("  [%s] %s", name, T("ui.item_queued"))
		case engine.Downloading:
			return fmt.Sprintf("  [%s] %s", name, count)
		case engine.Done:
			return fmt.Sprintf("✓ %s (%s)", j.FilePath, count)
		}
	}
	switch j.State {
	case engine.Queued:
		return fmt.Sprintf("  [%s] %s", name, T("ui.item_queued"))
	case engine.Downloading:
		s := fmt.Sprintf("  [%s] %5s %%", name, decimal(j.Percent))
		if j.Speed > 0 {
			s += "  " + T("ui.per_second", "size", decimal(j.Speed/1e6)+" "+T("ui.unit_mb"))
		}
		if j.ETA > 0 {
			s += "  " + T("ui.time_left", "time", T("ui.seconds", "s", strconv.Itoa(j.ETA)))
		}
		return s
	case engine.Processing:
		return fmt.Sprintf("  [%s] %s", name, T("ui.processing"))
	case engine.Done:
		return fmt.Sprintf("✓ %s", j.FilePath)
	case engine.Canceled:
		return fmt.Sprintf("✗ [%s] %s", name, T("ui.item_canceled"))
	default:
		if j.Problem != nil {
			return fmt.Sprintf("✗ [%s] %s", name, problemText(j.Problem))
		}
		return fmt.Sprintf("✗ [%s] %s", name, j.Error)
	}
}

// streamLine describes a stream the crow's nest caught.
func streamLine(st engine.Stream) string {
	parts := []string{map[string]string{"hls": T("ui.stream_hls"), "dash": T("ui.stream_dash"), "video": T("ui.stream_video"), "audio": T("ui.stream_audio")}[st.Kind]}
	switch {
	case st.Live:
		parts = append(parts, T("ui.live"))
	case st.Duration > 0:
		parts = append(parts, st.DurationString)
	}
	if st.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dp", st.Height))
	}
	return strings.Join(parts, ", ")
}

// Lang is the language the terminal speaks: Settings' choice, else the
// system's (LC_ALL, LC_MESSAGES, LANG).
var Lang = i18n.Default

// T is the message key in Lang, with its arguments as name, value pairs.
func T(key string, args ...string) string { return i18n.New(key, args...).In(Lang) }

// decimal is f to one decimal place, with the comma French and Spanish use.
func decimal(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if base, _, _ := strings.Cut(Lang, "-"); base == "fr" || base == "es" {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// SetLanguage picks Lang from the setting, or the environment if it's "".
func SetLanguage(setting string) {
	if setting != "" {
		Lang = setting
		return
	}
	Lang = i18n.Match(os.Getenv("LC_ALL"), os.Getenv("LC_MESSAGES"), os.Getenv("LANG"))
}

// Explained is a failure in the user's language, with what the tool said
// on the next line when that says more.
func Explained(err error) string { return problemText(i18n.From(err)) }

func problemText(p *i18n.Error) string {
	msg := p.In(Lang)
	if p.Detail != "" && p.Detail != msg && p.Key != "error.other" {
		msg += "\n    " + p.Detail
	}
	return msg
}
