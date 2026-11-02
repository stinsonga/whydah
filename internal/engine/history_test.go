package engine

import (
	"os"
	"path/filepath"
	"testing"

	"whydah/internal/events"
	"whydah/internal/tools"
)

func TestHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	saved := filepath.Join(dir, "Harbour Lights.mp4")
	if err := os.WriteFile(saved, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	newEngine := func() *Engine {
		hub := events.NewHub()
		e := New(nil, tools.New(dir, hub, func() string { return "stable" }), hub)
		e.KeepHistory(path)
		return e
	}

	e := newEngine()
	e.history.add(Job{URL: "https://vimeo.com/48213", Title: "Harbour Lights", FilePath: saved})
	e.history.add(Job{URL: "https://cdn.example.com/master.m3u8", Page: "https://films.example.com/storm", Title: "Storm at Sea", FilePath: filepath.Join(dir, "gone.mp4")})
	e.history.add(Job{URL: "https://youtube.com/watch?v=1", Title: "Track One", GroupTitle: "Sea Shanties", Audio: true, FilePath: filepath.Join(dir, "01 - Track One.mp3")})

	// Reloaded from the file, newest first; searched by every word.
	e = newEngine()
	all, total := e.History("", 10)
	if total != 3 || all[0].Title != "Track One" || all[2].Title != "Harbour Lights" {
		t.Fatalf("all: %d %+v", total, all)
	}
	if !all[2].Exists || all[1].Exists {
		t.Errorf("exists: %v %v, want true false", all[2].Exists, all[1].Exists)
	}
	for q, want := range map[string]string{
		"harbour":        "Harbour Lights",
		"films.example":  "Storm at Sea", // the page a stream was found on
		"shanties track": "Track One",    // the playlist, and every word
		"vimeo 48213":    "Harbour Lights",
	} {
		got, n := e.History(q, 10)
		if n != 1 || got[0].Title != want {
			t.Errorf("search %q: %d results %+v, want %s", q, n, got, want)
		}
	}
	if _, n := e.History("harbour storm", 10); n != 0 {
		t.Errorf("words are all required; got %d", n)
	}
	if got, n := e.History("", 1); n != 3 || len(got) != 1 {
		t.Errorf("limit: %d of %d", len(got), n)
	}

	// Removing one, then all, is saved.
	e.ForgetHistory(all[1].ID)
	if _, n := newEngine().History("", 10); n != 2 {
		t.Errorf("after removing one: %d", n)
	}
	e.ForgetHistory()
	if _, n := newEngine().History("", 10); n != 0 {
		t.Errorf("after clearing: %d", n)
	}

	// Terminal runs keep no history.
	hub := events.NewHub()
	cli := New(nil, tools.New(dir, hub, func() string { return "stable" }), hub)
	cli.history.add(Job{Title: "x"})
	if _, n := cli.History("", 10); n != 0 {
		t.Error("an engine without KeepHistory recorded a download")
	}
}
