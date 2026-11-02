package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The history is everything salvaged, kept after the hold is cleared, to
// search and to salvage again. It's one JSON line per download in a file
// next to the settings, appended to as downloads finish.

// HistoryEntry is one finished download.
type HistoryEntry struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`            // what was downloaded
	Page      string    `json:"page,omitempty"` // the page it was found on, for a stream
	Title     string    `json:"title"`
	Thumbnail string    `json:"thumbnail,omitempty"`
	Kind      string    `json:"kind,omitempty"` // "photos", or "" for video/audio
	Audio     bool      `json:"audio,omitempty"`
	Group     string    `json:"group,omitempty"` // the playlist it was part of
	FilePath  string    `json:"file_path"`
	Files     int       `json:"files,omitempty"` // photos saved
	Finished  time.Time `json:"finished"`
	Exists    bool      `json:"exists"` // filled in when listed: the file is still there
}

const maxHistory = 10000 // the oldest go once there are more

type history struct {
	mu      sync.Mutex
	path    string // "" when it isn't kept (terminal runs)
	entries []HistoryEntry
	seq     int
}

// KeepHistory loads the history saved at path and records finished
// downloads there from now on.
func (e *Engine) KeepHistory(path string) { e.history.load(path) }

func (h *history) load(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.path = path
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("reading history", "err", err)
		}
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e HistoryEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			h.entries = append(h.entries, e)
		}
	}
	if len(h.entries) > maxHistory {
		h.entries = h.entries[len(h.entries)-maxHistory:]
		h.rewrite()
	}
}

// add records a finished job.
func (h *history) add(j Job) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.path == "" {
		return
	}
	h.seq++
	e := HistoryEntry{
		ID: strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(h.seq), URL: j.URL, Page: j.Page, Title: j.Title,
		Thumbnail: j.Thumbnail, Kind: j.Kind, Audio: j.Audio, Group: j.GroupTitle, FilePath: j.FilePath, Files: j.Files,
		Finished: time.Now(),
	}
	h.entries = append(h.entries, e)
	data, _ := json.Marshal(e)
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = f.Write(append(data, '\n'))
		f.Close()
	}
	if err != nil {
		slog.Warn("saving history", "err", err)
	}
	if len(h.entries) > maxHistory+maxHistory/10 { // trim in steps, not on every add
		h.entries = h.entries[len(h.entries)-maxHistory:]
		h.rewrite()
	}
}

// rewrite saves the whole history, after entries were removed. Call with mu held.
func (h *history) rewrite() {
	if h.path == "" {
		return
	}
	var b strings.Builder
	for _, e := range h.entries {
		data, _ := json.Marshal(e)
		b.Write(data)
		b.WriteByte('\n')
	}
	tmp := h.path + ".tmp"
	err := os.WriteFile(tmp, []byte(b.String()), 0o644)
	if err == nil {
		err = os.Rename(tmp, h.path)
	}
	if err != nil {
		slog.Warn("saving history", "err", err)
	}
}

// History lists past downloads, newest first, that match every word of
// query (in the title, the links, the playlist or the file name), up to
// limit. total is how many matched.
func (e *Engine) History(query string, limit int) (entries []HistoryEntry, total int) {
	words := strings.Fields(strings.ToLower(query))
	entries = []HistoryEntry{} // a list, even when empty
	h := &e.history
	h.mu.Lock()
	for i := len(h.entries) - 1; i >= 0; i-- {
		en := h.entries[i]
		text := strings.ToLower(strings.Join([]string{en.Title, en.URL, en.Page, en.Group, filepath.Base(en.FilePath)}, " "))
		match := true
		for _, w := range words {
			if !strings.Contains(text, w) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		total++
		if len(entries) < limit {
			entries = append(entries, en)
		}
	}
	h.mu.Unlock()
	for i := range entries {
		_, err := os.Stat(entries[i].FilePath)
		entries[i].Exists = entries[i].FilePath != "" && err == nil
	}
	return entries, total
}

// HistoryEntry finds one past download.
func (e *Engine) HistoryEntry(id string) (HistoryEntry, bool) {
	h := &e.history
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, en := range h.entries {
		if en.ID == id {
			return en, true
		}
	}
	return HistoryEntry{}, false
}

// ForgetHistory removes past downloads from the history (not their files):
// the ones with these ids, or all of them if none are given.
func (e *Engine) ForgetHistory(ids ...string) {
	h := &e.history
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(ids) == 0 {
		h.entries = nil
	} else {
		drop := map[string]bool{}
		for _, id := range ids {
			drop[id] = true
		}
		kept := h.entries[:0]
		for _, en := range h.entries {
			if !drop[en.ID] {
				kept = append(kept, en)
			}
		}
		h.entries = kept
	}
	h.rewrite()
}
