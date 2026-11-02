// Package cli is the terminal mode: salvage (download), spyglass (look
// without downloading) and refit (update the tools).
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"whydah/internal/engine"
	"whydah/internal/events"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

type printer struct {
	pending   map[string]bool
	lastPrint map[string]time.Time
	lastState map[string]engine.State
	lastStage string
	failed    int
}

func (p *printer) handle(ev events.Event) {
	switch d := ev.Data.(type) {
	case tools.Status:
		if d.Stage != "" && d.Stage != p.lastStage {
			p.lastStage = d.Stage
			fmt.Println("·", d.Stage)
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
				fmt.Println("✗", err)
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
		fmt.Println("!", w)
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
			fmt.Printf("✗ %s: %s\n", url, explained(err))
			p.failed++
			continue
		}
		if linked && len(md.Links) > 0 {
			fmt.Printf("→ checking %s linked from %s…\n", plural(len(md.Links), "page"), md.Title)
			if md, err = eng.LinkedPlaylist(ctx, url, md.Title, md.Links); err != nil {
				if ctx.Err() != nil {
					return 130
				}
				fmt.Printf("✗ %s: %s\n", url, explained(err))
				p.failed++
				continue
			}
		} else if len(md.Links) > 0 {
			fmt.Printf("  (%s linked from this page; add --linked to salvage them)\n", plural(len(md.Links), "possible video"))
		}
		for _, st := range md.Streams {
			fmt.Printf("→ stream: %s (%s)\n", st.Title, streamLine(st))
			j := eng.Download(engine.Request{URL: st.URL, Audio: audio || st.Kind == "audio", Title: st.Title,
				Thumbnail: st.Thumbnail, Referer: st.Referer, Origin: st.Origin, Page: url})
			p.pending[j.ID] = true
		}
		if md.Kind == "links" || md.Kind == "streams" {
			continue
		}
		if md.Kind == "photos" {
			fmt.Printf("→ photos: %s (%s)\n", md.Title, photoCount(md))
			j := eng.DownloadPhotos(engine.PhotoRequest{URL: url, Title: md.Title, Source: md.Source,
				Total: len(md.Photos), Truncated: md.Truncated, Items: md.Photos})
			p.pending[j.ID] = true
			continue
		}
		if md.Kind == "playlist" {
			fmt.Printf("→ playlist: %s (%d items)\n", md.Title, len(md.Entries))
			items := make([]engine.Item, len(md.Entries))
			for i, en := range md.Entries {
				items[i] = engine.Item{Index: en.Index, URL: en.URL, Title: en.Title, Thumbnail: en.Thumbnail}
			}
			for _, j := range eng.DownloadPlaylist(md.Title, len(md.Entries), audio, items) {
				p.pending[j.ID] = true
			}
			continue
		}
		fmt.Printf("→ %s (%s, %s)\n", md.Title, md.Uploader, md.DurationString)
		j := eng.Download(engine.Request{URL: url, Audio: audio, Title: md.Title, Thumbnail: md.Thumbnail})
		p.pending[j.ID] = true
	}

	// The hub may drop events if we fell behind while inspecting, so also poll.
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for len(p.pending) > 0 {
		select {
		case <-ctx.Done():
			fmt.Println("canceled") // main's eng.Shutdown stops the downloads
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
			fmt.Printf("✗ %s: %s\n", url, explained(err))
			failed++
			continue
		}
		fmt.Println(url)
		switch md.Kind {
		case "photos":
			fmt.Printf("  photos: %s (%s, %s)\n", md.Title, photoCount(md), map[string]string{"page": "found on the page", "gallery-dl": "via gallery-dl", "browser": "from the crow's nest"}[md.Source])
			for _, ph := range md.Photos {
				size := ""
				if ph.Width > 0 {
					size = fmt.Sprintf(" %d×%d", ph.Width, ph.Height)
				}
				fmt.Printf("  %4d%s  %s\n", ph.Index, size, ph.URL)
			}
		case "playlist":
			fmt.Printf("  playlist: %s (%d items)\n", md.Title, len(md.Entries))
			for _, en := range md.Entries {
				fmt.Printf("  %4d  %s  [%s]\n", en.Index, en.Title, en.DurationString)
			}
		case "links", "streams":
		default:
			fmt.Printf("  video: %s\n  by %s · %s\n", md.Title, md.Uploader, md.DurationString)
			if md.PlaylistURL != "" {
				fmt.Printf("  from playlist: %s\n", md.PlaylistURL)
			}
		}
		for i, st := range md.Streams {
			fmt.Printf("  stream %d: %s (%s)\n        %s\n", i+1, st.Title, streamLine(st), st.URL)
		}
		if len(md.Links) == 0 {
			continue
		}
		if !linked {
			fmt.Printf("  %s linked (add --linked to check them):\n", plural(len(md.Links), "possible video"))
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
			fmt.Printf("  linked videos: %v\n", err)
			continue
		}
		fmt.Printf("  linked videos: %d of %d links\n", len(pl.Entries), len(md.Links))
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

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func photoCount(md engine.Metadata) string {
	n := fmt.Sprintf("%d photos", len(md.Photos))
	if len(md.Photos) == 1 {
		n = "1 photo"
	}
	if md.Truncated {
		n = "more than " + n
	}
	return n
}

// hintUpdate suggests a refit after failures, if newer tools exist.
func hintUpdate(ctx context.Context, tm *tools.Manager) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if tm.Check(cctx) != nil {
		return
	}
	if u := tm.Status().Update; u.Available {
		fmt.Printf("\nNewer tools are available and may fix this: %s\nRun: whydah refit\n", pendingList(u.Pending))
	}
}

func pendingList(pending []tools.PendingUpdate) string {
	var parts []string
	for _, p := range pending {
		parts = append(parts, fmt.Sprintf("%s %s → %s", p.Tool, p.Current, p.Latest))
	}
	return strings.Join(parts, ", ")
}

// Refit updates yt-dlp and gallery-dl and returns a process exit code.
func Refit(ctx context.Context, tm *tools.Manager) int {
	if _, err := tm.Wait(ctx); err != nil {
		fmt.Println("✗", err)
		return 1
	}
	before := installed(tm.Status())
	fmt.Println("Refitting: updating yt-dlp and gallery-dl…")
	err := tm.Install(ctx)
	after := installed(tm.Status())
	for _, tool := range []string{"yt-dlp", "gallery-dl"} {
		switch {
		case after[tool] == "":
		case after[tool] != before[tool]:
			fmt.Printf("✓ %s updated: %s → %s\n", tool, before[tool], after[tool])
		default:
			fmt.Printf("✓ %s is up to date (%s)\n", tool, after[tool])
		}
	}
	if err != nil {
		fmt.Println("✗", err)
		return 1
	}
	return 0
}

func installed(st tools.Status) map[string]string {
	gdl := st.GalleryDLVer
	if release, build, ok := strings.Cut(gdl, ":"); ok {
		gdl = release + " (build " + build + ")"
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
		count := fmt.Sprintf("%d photos", j.Files)
		if j.FileTotal > 0 {
			count = fmt.Sprintf("%d/%d photos", j.Files, j.FileTotal)
		}
		switch j.State {
		case engine.Queued:
			return fmt.Sprintf("  [%s] queued", name)
		case engine.Downloading:
			return fmt.Sprintf("  [%s] %s", name, count)
		case engine.Done:
			return fmt.Sprintf("✓ %s (%s)", j.FilePath, count)
		}
	}
	switch j.State {
	case engine.Queued:
		return fmt.Sprintf("  [%s] queued", name)
	case engine.Downloading:
		s := fmt.Sprintf("  [%s] %5.1f%%", name, j.Percent)
		if j.Speed > 0 {
			s += fmt.Sprintf("  %.1f MB/s", j.Speed/1e6)
		}
		if j.ETA > 0 {
			s += fmt.Sprintf("  ETA %ds", j.ETA)
		}
		return s
	case engine.Processing:
		return fmt.Sprintf("  [%s] processing…", name)
	case engine.Done:
		return fmt.Sprintf("✓ %s", j.FilePath)
	case engine.Canceled:
		return fmt.Sprintf("✗ [%s] canceled", name)
	default:
		if j.Problem != nil {
			return fmt.Sprintf("✗ [%s] %s", name, problemText(j.Problem))
		}
		return fmt.Sprintf("✗ [%s] %s", name, j.Error)
	}
}

// streamLine describes a stream the crow's nest caught.
func streamLine(st engine.Stream) string {
	parts := []string{map[string]string{"hls": "HLS", "dash": "DASH", "video": "video file", "audio": "audio file"}[st.Kind]}
	switch {
	case st.Live:
		parts = append(parts, "live")
	case st.Duration > 0:
		parts = append(parts, st.DurationString)
	}
	if st.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dp", st.Height))
	}
	return strings.Join(parts, ", ")
}

// Lang is the language failures are explained in: Settings' choice, else
// the system's (LC_ALL, LC_MESSAGES, LANG).
var Lang = i18n.Default

// SetLanguage picks Lang from the setting, or the environment if it's "".
func SetLanguage(setting string) {
	if setting != "" {
		Lang = setting
		return
	}
	Lang = i18n.Match(os.Getenv("LC_ALL"), os.Getenv("LC_MESSAGES"), os.Getenv("LANG"))
}

// explained is a failure in the user's language, with what the tool said
// on the next line when that says more.
func explained(err error) string { return problemText(i18n.From(err)) }

func problemText(p *i18n.Error) string {
	msg := p.In(Lang)
	if p.Detail != "" && p.Detail != msg && p.Key != "error.other" {
		msg += "\n    " + p.Detail
	}
	return msg
}
