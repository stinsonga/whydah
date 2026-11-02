// Command whydah salvages video, audio and photos from the web: yt-dlp for
// video and audio, gallery-dl or a page scan for photos. It opens a browser
// UI by default; the salvage, spyglass and refit subcommands work in the terminal.
//
// Named after Samuel Bellamy's flagship.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"whydah/internal/cli"
	"whydah/internal/clipboard"
	"whydah/internal/config"
	"whydah/internal/engine"
	"whydah/internal/events"
	"whydah/internal/tools"
	"whydah/internal/web"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const defaultPort = 47823

func main() {
	os.Exit(run())
}

func run() int {
	audio := flag.Bool("audio", false, "CLI: extract audio only")
	linked := flag.Bool("linked", false, "CLI: check the videos a page's thumbnails link to (spyglass lists them, salvage downloads them)")
	crowsNest := flag.Bool("crows-nest", false, "CLI: find photos by loading each page in a hidden browser, for pages built by scripts")
	output := flag.String("output", "", "download folder (overrides settings for this run)")
	browser := flag.String("browser", "", "use cookies from this browser for this run (chrome, firefox, safari, ..., or whydah for Whydah's sign-in browser)")
	port := flag.Int("port", defaultPort, "UI port (falls back to a random port if busy)")
	noBrowser := flag.Bool("no-browser", false, "don't open the UI in a browser")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `Whydah salvages video, audio and photos from the web.

Usage:
  whydah                          open the UI in your browser
  whydah salvage [flags] URL...   download everything at each link (or just: whydah URL...)
  whydah spyglass URL...          show what each link has, without downloading
  whydah refit                    update the helper tools to their latest releases
  whydah sign-in [URL]            open Whydah's own browser to sign in to sites (at URL)

Flags:
`)
		flag.PrintDefaults()
	}
	// Flags may come before or after the subcommand.
	flag.Parse()
	addUserPaths()
	command, urls := "", flag.Args()
	if len(urls) > 0 {
		switch urls[0] {
		case "salvage", "spyglass", "refit", "sign-in", "help":
			command = urls[0]
			_ = flag.CommandLine.Parse(urls[1:])
			urls = flag.Args()
		default:
			command = "salvage"
		}
	}
	if *showVersion {
		fmt.Println("whydah", version)
		return 0
	}
	switch {
	case command == "help":
		flag.Usage()
		return 0
	case (command == "salvage" || command == "spyglass") && len(urls) == 0:
		fmt.Fprintf(os.Stderr, "whydah %s needs at least one link\n", command)
		return 2
	case command == "refit" && len(urls) > 0:
		fmt.Fprintln(os.Stderr, "whydah refit takes no links")
		return 2
	case command == "sign-in" && len(urls) > 1:
		fmt.Fprintln(os.Stderr, "whydah sign-in takes at most one link")
		return 2
	}

	appDir, err := config.AppDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	cacheDir, err := config.CacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	closeLog := setupLogging(filepath.Join(appDir, "app.log"))
	defer closeLog()
	slog.Info("starting", "version", version, "args", os.Args[1:])

	settings, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error loading settings:", err)
		return 1
	}
	if *output != "" || *browser != "" {
		settings.Override(func(s *config.Settings) {
			if *output != "" {
				if abs, err := filepath.Abs(*output); err == nil {
					s.OutputDir = abs
				}
			}
			if *browser != "" {
				s.BrowserCookies = strings.ToLower(*browser)
			}
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token, err := loadToken(filepath.Join(appDir, "ui-token"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	running := (command == "" || command == "refit" || command == "sign-in") && web.PingExisting(*port)
	if command == "sign-in" && running {
		cli.SetLanguage(settings.Get().Language)
		if err := web.SignInExisting(*port, token, strings.Join(urls, "")); err != nil {
			fmt.Fprintln(os.Stderr, "✗", cli.Explained(err))
			return 1
		}
		fmt.Println("Opened the sign-in window of the Whydah that's running. Close it when you're done.")
		return 0
	}
	if command == "refit" && running {
		// The update lock is per process, so let the running app do it safely.
		fmt.Println("Whydah is running; use Refit in its window, or quit it first.")
		return 1
	}
	// A second launch just reopens the UI of the instance already running.
	if running {
		url := fmt.Sprintf("http://127.0.0.1:%d/#t=%s", *port, token)
		fmt.Println("Whydah is already running:", url)
		if !*noBrowser {
			_ = web.OpenPath(url)
		}
		return 0
	}

	if command == "" && openedAsApp() {
		if err := serveInBackground(); err != nil {
			slog.Error("starting server", "err", err)
			return 1
		}
		return 0
	}

	hub := events.NewHub()
	tm := tools.New(cacheDir, hub, func() string { return settings.Get().UpdateChannel })
	tm.Start(ctx)
	if command == "refit" {
		return cli.Refit(ctx, tm)
	}
	cli.SetLanguage(settings.Get().Language)
	eng := engine.New(settings, tm, hub)
	defer eng.Shutdown(10 * time.Second)

	switch command {
	case "salvage":
		return cli.Salvage(ctx, eng, tm, hub, urls, *audio, *linked, engine.InspectOptions{CrowsNest: *crowsNest})
	case "spyglass":
		return cli.Spyglass(ctx, eng, tm, hub, urls, *linked, engine.InspectOptions{CrowsNest: *crowsNest})
	case "sign-in":
		return cli.SignIn(ctx, eng, hub, strings.Join(urls, ""))
	}

	eng.KeepHistory(filepath.Join(appDir, "history.jsonl")) // first: resumed downloads that finish go in it
	eng.Persist(filepath.Join(appDir, "queue.json"))
	go clipboard.Watch(ctx, settings, hub)
	go tm.WatchForUpdates(ctx)
	srv := &web.Server{Settings: settings, Engine: eng, Tools: tm, Hub: hub, Token: token, Quit: stop}
	ln, err := srv.Listen(*port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("Whydah is running at", srv.URL())
	fmt.Println("Keep this window open while downloading. Press Ctrl+C (or Quit in the UI) to stop.")
	if !*noBrowser {
		if err := web.OpenPath(srv.URL()); err != nil {
			fmt.Println("Couldn't open a browser automatically; open the link above.")
		}
	}
	if err := srv.Serve(ctx, ln); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("Stopped.")
	return 0
}

// loadToken returns the per-install UI token, creating it on first run.
// It's stable across launches so a second launch can reopen the running UI.
func loadToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	return tok, os.WriteFile(path, []byte(tok), 0o600)
}

// setupLogging writes structured logs to path, keeping one previous file
// once it grows past 5 MB. Falls back to discarding logs if it can't open.
func setupLogging(path string) func() {
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	return func() { f.Close() }
}

// addUserPaths adds the usual Homebrew and MacPorts folders to PATH. Opened
// from Finder, the app gets launchd's bare PATH and would miss a system
// ffmpeg, deno or gallery-dl that a shell would find.
func addUserPaths() {
	if runtime.GOOS != "darwin" {
		return
	}
	path := os.Getenv("PATH")
	have := map[string]bool{}
	for _, d := range filepath.SplitList(path) {
		have[d] = true
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin"} {
		if !have[d] {
			path += string(os.PathListSeparator) + d
		}
	}
	os.Setenv("PATH", path)
}
