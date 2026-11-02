// Package web serves the browser UI and its JSON/SSE API on localhost.
//
// Any web page can send requests to 127.0.0.1, so every API call must carry
// the per-install token (only known to pages we open), and the Host header is
// checked to block DNS-rebinding.
package web

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"whydah/internal/config"
	"whydah/internal/engine"
	"whydah/internal/events"
	"whydah/internal/i18n"
	"whydah/internal/tools"
)

//go:embed icon.png
var iconPNG []byte

//go:embed index.html
var indexHTML []byte

// appID identifies us to a second launch's /api/ping.
const appID = "whydah"

type Server struct {
	Settings *config.Store
	Engine   *engine.Engine
	Tools    *tools.Manager
	Hub      *events.Hub
	Token    string
	Quit     func() // called by the UI's Quit button

	port int
}

// Listen binds preferredPort on loopback, falling back to a random port.
func (s *Server) Listen(preferredPort int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferredPort))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
	}
	s.port = ln.Addr().(*net.TCPAddr).Port
	return ln, nil
}

func (s *Server) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/#t=%s", s.port, s.Token)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		_, _ = w.Write(iconPNG)
	})
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"app": appID}) })
	mux.HandleFunc("GET /api/state", s.auth(s.state))
	mux.HandleFunc("GET /api/i18n", s.auth(s.messages))
	mux.HandleFunc("GET /api/events", s.auth(s.events))
	mux.HandleFunc("POST /api/inspect", s.auth(s.inspect))
	mux.HandleFunc("POST /api/download", s.auth(s.download))
	mux.HandleFunc("POST /api/download-playlist", s.auth(s.downloadPlaylist))
	mux.HandleFunc("POST /api/download-photos", s.auth(s.downloadPhotos))
	mux.HandleFunc("POST /api/check-links", s.auth(s.checkLinks))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.auth(s.cancel))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.auth(s.retry))
	mux.HandleFunc("POST /api/groups/{id}/cancel", s.auth(s.cancelGroup))
	mux.HandleFunc("POST /api/jobs/{id}/reveal", s.auth(s.reveal))
	mux.HandleFunc("POST /api/jobs/clear", s.auth(s.clear))
	mux.HandleFunc("GET /api/history", s.auth(s.history))
	mux.HandleFunc("POST /api/history/{id}/reveal", s.auth(s.revealHistory))
	mux.HandleFunc("POST /api/history/{id}/forget", s.auth(s.forgetHistory))
	mux.HandleFunc("POST /api/history/forget", s.auth(s.forgetHistory))
	mux.HandleFunc("POST /api/open-folder", s.auth(s.openFolder))
	mux.HandleFunc("PUT /api/settings", s.auth(s.putSettings))
	mux.HandleFunc("POST /api/signin", s.auth(s.signIn))
	mux.HandleFunc("POST /api/signout", s.auth(s.signOut))
	mux.HandleFunc("GET /api/image", s.auth(s.image))
	mux.HandleFunc("POST /api/tools/check", s.auth(s.checkUpdate))
	mux.HandleFunc("POST /api/tools/update", s.auth(s.installUpdate))
	mux.HandleFunc("POST /api/quit", s.auth(s.quit))

	srv := &http.Server{Handler: s.checkHost(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) checkHost(next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", s.port): true,
		fmt.Sprintf("localhost:%d", s.port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Token")
		if tok == "" {
			tok = r.URL.Query().Get("t") // EventSource can't set headers
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
			http.Error(w, "unauthorized: reopen the app to get a fresh link", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

type stateResponse struct {
	Settings          config.Settings     `json:"settings"`
	Jobs              []engine.Job        `json:"jobs"`
	Tools             tools.Status        `json:"tools"`
	Browsers          []string            `json:"browsers"`           // detected installed
	SupportedBrowsers []string            `json:"supported_browsers"` // all yt-dlp knows
	AudioFormats      []string            `json:"audio_formats"`
	NameTokens        []string            `json:"name_tokens"`
	VideoQualities    []string            `json:"video_qualities"`
	UpdateChannels    []string            `json:"update_channels"`
	SignIn            engine.SignInStatus `json:"signin"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, stateResponse{
		Settings:          s.Settings.Get(),
		Jobs:              s.Engine.Jobs(),
		Tools:             s.Tools.Status(),
		Browsers:          config.DetectBrowsers(),
		SupportedBrowsers: config.SupportedBrowsers,
		AudioFormats:      config.AudioFormats,
		VideoQualities:    config.VideoQualities,
		UpdateChannels:    config.UpdateChannels,
		NameTokens:        config.NameTokens,
		SignIn:            s.Engine.SignInStatus(),
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch, unsubscribe := s.Hub.Subscribe()
	defer unsubscribe()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		flusher.Flush()
	}
}

func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
		engine.InspectOptions
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.URL == "" {
		httpError(w, http.StatusBadRequest, "url is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	md, err := s.Engine.Inspect(ctx, req.URL, req.InspectOptions)
	if err != nil {
		problemError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, md)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL       string `json:"url"`
		Audio     bool   `json:"audio"`
		Title     string `json:"title"`
		Thumbnail string `json:"thumbnail"`
		Referer   string `json:"referer"` // a stream from the crow's nest
		Origin    string `json:"origin"`
		Page      string `json:"page"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.URL == "" {
		httpError(w, http.StatusBadRequest, "url is required")
		return
	}
	writeJSON(w, s.Engine.Download(engine.Request{URL: req.URL, Audio: req.Audio, Title: req.Title, Thumbnail: req.Thumbnail,
		Referer: req.Referer, Origin: req.Origin, Page: req.Page}))
}

func (s *Server) downloadPlaylist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string        `json:"title"`
		Total int           `json:"total"`
		Audio bool          `json:"audio"`
		Items []engine.Item `json:"items"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		httpError(w, http.StatusBadRequest, "no items selected")
		return
	}
	for _, it := range req.Items {
		if it.URL == "" {
			httpError(w, http.StatusBadRequest, "every item needs a url")
			return
		}
	}
	writeJSON(w, s.Engine.DownloadPlaylist(req.Title, req.Total, req.Audio, req.Items))
}

// checkLinks runs yt-dlp on the linked pages an inspection found and returns
// the videos as a playlist. Progress goes out as links_progress events.
func (s *Server) checkLinks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL   string               `json:"url"`
		Title string               `json:"title"`
		Links []engine.LinkedVideo `json:"links"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !isHTTP(req.URL) || len(req.Links) == 0 {
		httpError(w, http.StatusBadRequest, "url and links are required")
		return
	}
	for _, l := range req.Links {
		if !isHTTP(l.URL) {
			httpError(w, http.StatusBadRequest, "every link needs an http(s) url")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	md, err := s.Engine.LinkedPlaylist(ctx, req.URL, req.Title, req.Links)
	if err != nil {
		problemError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, md)
}

func (s *Server) downloadPhotos(w http.ResponseWriter, r *http.Request) {
	var req engine.PhotoRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !isHTTP(req.URL) || (req.Source != "gallery-dl" && req.Source != "page" && req.Source != "browser") {
		httpError(w, http.StatusBadRequest, "url and source are required")
		return
	}
	if len(req.Items) == 0 {
		httpError(w, http.StatusBadRequest, "no photos selected")
		return
	}
	for _, it := range req.Items {
		if it.Index < 1 || (req.Source == "page" && !isHTTP(it.URL)) {
			httpError(w, http.StatusBadRequest, "every photo needs an index and an http(s) url")
			return
		}
	}
	writeJSON(w, s.Engine.DownloadPhotos(req))
}

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Engine.Retry(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusConflict, "only failed or canceled downloads can be retried")
		return
	}
	writeJSON(w, j)
}

func (s *Server) cancelGroup(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]int{"canceled": s.Engine.CancelGroup(r.PathValue("id"))})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if !s.Engine.Cancel(r.PathValue("id")) {
		httpError(w, http.StatusNotFound, "no running job with that id")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) reveal(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Engine.Job(r.PathValue("id"))
	if !ok || j.FilePath == "" {
		httpError(w, http.StatusNotFound, "no file for that job")
		return
	}
	if err := RevealFile(j.FilePath); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// history lists past downloads matching ?q=, newest first.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	entries, total := s.Engine.History(r.URL.Query().Get("q"), 200)
	writeJSON(w, map[string]any{"entries": entries, "total": total})
}

func (s *Server) revealHistory(w http.ResponseWriter, r *http.Request) {
	e, ok := s.Engine.HistoryEntry(r.PathValue("id"))
	if !ok || e.FilePath == "" {
		httpError(w, http.StatusNotFound, "no such download")
		return
	}
	if err := RevealFile(e.FilePath); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// forgetHistory removes one past download from the history, or with no
// id, all of them. The files stay.
func (s *Server) forgetHistory(w http.ResponseWriter, r *http.Request) {
	if id := r.PathValue("id"); id != "" {
		s.Engine.ForgetHistory(id)
	} else {
		s.Engine.ForgetHistory()
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	s.Engine.ClearFinished()
	writeJSON(w, s.Engine.Jobs())
}

func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	dir := s.Settings.Get().OutputDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := OpenPath(dir); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in config.Settings
	if !readJSON(w, r, &in) {
		return
	}
	if !filepath.IsAbs(in.OutputDir) && in.OutputDir != "" && in.OutputDir[0] != '~' {
		httpError(w, http.StatusBadRequest, "download folder must be an absolute path")
		return
	}
	if in.NameTemplate != "" {
		if err := config.CheckNameTemplate(in.NameTemplate); err != nil {
			problemError(w, http.StatusBadRequest, err)
			return
		}
	}
	before := s.Settings.Get()
	out, err := s.Settings.Update(in)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Engine.SettingsChanged()
	if out.UpdateChannel != before.UpdateChannel {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = s.Tools.Check(ctx)
		}()
	}
	writeJSON(w, out)
}

// signIn opens the sign-in window, at the page given if any; its status
// arrives as signin_status events.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if err := s.Engine.SignIn(ctx, in.URL); err != nil {
		problemError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, s.Engine.SignInStatus())
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.SignOut(); err != nil {
		problemError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, s.Engine.SignInStatus())
}

// image passes on an image found with the sign-in browser (see
// engine.FetchImage), so the UI can show it. Only images are passed on, and
// never SVG, since anything else could run script as this page.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := s.Engine.FetchImage(ctx, r.URL.Query().Get("u"), r.URL.Query().Get("page"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ctype, "image/") || ctype == "image/svg+xml" {
		http.Error(w, "not an image: "+resp.Status+" "+ctype, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 50<<20))
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_ = s.Tools.Check(ctx) // a failure is reported in the status
	writeJSON(w, s.Tools.Status())
}

// installUpdate starts the update and returns; progress arrives as tools_status events.
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	go func() { _ = s.Tools.Install(context.Background()) }()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func (s *Server) quit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	if s.Quit != nil {
		go func() {
			time.Sleep(200 * time.Millisecond) // let the response flush
			s.Quit()
		}()
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20) // big playlists
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// messages sends the UI its messages: in the language chosen in Settings,
// or else the best match for the browser's (?lang=fr-CA,fr,en).
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	prefs := strings.Split(r.URL.Query().Get("lang"), ",")
	if r.URL.Query().Get("lang") == "" {
		prefs = strings.Split(r.Header.Get("Accept-Language"), ",")
	}
	auto := i18n.Match(prefs...)
	lang := firstNonEmpty(s.Settings.Get().Language, auto)
	writeJSON(w, map[string]any{"lang": lang, "auto": auto, "languages": i18n.Languages(), "messages": i18n.Messages(lang)})
}

// problemError sends a failure explained: "problem" for the UI to show in
// the user's language, and "error" in English for anything else.
func problemError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "problem": i18n.From(err)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("writing response", "err", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// PingExisting checks whether another instance is already serving on port.
func PingExisting(port int) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		App string `json:"app"`
	}
	return json.NewDecoder(resp.Body).Decode(&body) == nil && body.App == appID
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
