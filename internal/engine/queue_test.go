package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"whydah/internal/events"
	"whydah/internal/tools"
)

func TestPersistResumesQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")
	created := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	saved := []savedJob{
		{Job: Job{ID: "3", URL: "https://example.com/a", Title: "A", State: Done, Percent: 100, FilePath: "/out/A.mp4", Created: created}},
		{
			Job: Job{
				ID: "5", URL: "https://example.com/g", Title: "G", State: Downloading, Percent: 40, Files: 2,
				Group: "g7", GroupTitle: "List", Index: 2, Kind: "photos", Created: created.Add(time.Minute),
			},
			Pad:    3,
			Photos: &PhotoRequest{URL: "https://example.com/g", Source: "page", Items: []Photo{{Index: 1, URL: "https://example.com/1.jpg"}}},
		},
	}
	data, _ := json.Marshal(saved)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// The tools never become ready, so the resumed job waits in the queue.
	hub := events.NewHub()
	e := New(nil, tools.New(dir, hub, func() string { return "stable" }), hub)
	e.Persist(path)

	jobs := e.Jobs()
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].State != Done || jobs[0].FilePath != "/out/A.mp4" {
		t.Errorf("finished job changed: %+v", jobs[0])
	}
	r := jobs[1]
	if r.State != Queued || r.Percent != 0 || r.Files != 0 || r.Group != "g7" {
		t.Errorf("resumed job not reset: %+v", r)
	}
	e.mu.Lock()
	j := e.jobs["5"]
	if j.pad != 3 || j.photos == nil || len(j.photos.Items) != 1 {
		t.Errorf("resumed job lost its request: pad %d, photos %+v", j.pad, j.photos)
	}
	e.mu.Unlock()

	if id := e.Download(Request{URL: "https://example.com/b"}).ID; id != "8" {
		t.Errorf("new job got ID %s, want 8 (after the saved group g7)", id)
	}

	// Quitting keeps unfinished jobs as still to do, not canceled.
	e.Shutdown(5 * time.Second)
	var after []savedJob
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Fatalf("saved %d jobs, want 3", len(after))
	}
	for _, s := range after[1:] {
		if s.State.Finished() {
			t.Errorf("job %s saved as %s at shutdown", s.ID, s.State)
		}
	}
	if after[1].Photos == nil || after[1].Pad != 3 {
		t.Errorf("photo request not saved: %+v", after[1])
	}
}

func TestPersistKeepsNewestFinished(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var saved []savedJob
	for i := range maxFinished + 20 {
		saved = append(saved, savedJob{Job: Job{ID: strconv.Itoa(i + 1), URL: "https://example.com/v", State: Done, Created: start.Add(time.Duration(i) * time.Minute)}})
	}
	data, _ := json.Marshal(saved)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	hub := events.NewHub()
	e := New(nil, tools.New(dir, hub, func() string { return "stable" }), hub)
	e.Persist(path)

	jobs := e.Jobs()
	if len(jobs) != maxFinished {
		t.Fatalf("kept %d finished jobs, want %d", len(jobs), maxFinished)
	}
	e.mu.Lock()
	_, oldest := e.jobs["1"]
	_, newest := e.jobs[strconv.Itoa(maxFinished+20)]
	e.mu.Unlock()
	if oldest || !newest {
		t.Errorf("kept the oldest %v, the newest %v; want the newest only", oldest, newest)
	}
	if id := e.Download(Request{URL: "https://example.com/b"}).ID; id != strconv.Itoa(maxFinished+21) {
		t.Errorf("new job got ID %s, want %d: IDs mustn't be reused", id, maxFinished+21)
	}
}
