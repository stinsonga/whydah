package engine

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The queue is kept in a file so that downloads queued or running when
// Whydah quit (or crashed) start again on the next launch, and finished ones
// stay listed until cleared. yt-dlp carries on from its .part files and photo
// jobs skip what's saved, so a resumed job doesn't start from scratch. Only
// the newest maxFinished finished jobs are kept; History has the rest.

const maxFinished = 500

// savedJob is a Job with the unexported fields a resumed or retried job needs.
type savedJob struct {
	Job
	Pad    int           `json:"pad,omitempty"`
	Photos *PhotoRequest `json:"photos,omitempty"`
}

// Persist loads the queue saved at path, restarts its unfinished jobs, and
// from then on saves the queue there whenever a job changes state.
func (e *Engine) Persist(path string) {
	var saved []savedJob
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("reading saved queue", "path", path, "err", err)
	}

	var resume []*Job
	e.mu.Lock()
	e.queuePath = path
	for _, s := range saved {
		j := s.Job
		j.pad, j.photos = s.Pad, s.Photos
		n, _ := strconv.Atoi(j.ID)
		g, _ := strconv.Atoi(strings.TrimPrefix(j.Group, "g"))
		e.seq = max(e.seq, n, g)
		if j.State.Finished() {
			e.jobs[j.ID] = &j
		} else {
			j.reset()
			resume = append(resume, &j)
		}
	}
	e.pruneFinished()
	e.mu.Unlock()
	go e.saver()

	for _, j := range resume {
		e.start(j)
	}
	if len(saved) > 0 {
		slog.Info("restored queue", "jobs", len(saved), "resumed", len(resume))
	}
}

// pruneFinished forgets the oldest finished jobs past maxFinished. e.mu must be held.
func (e *Engine) pruneFinished() {
	var finished []*Job
	for _, j := range e.jobs {
		if _, active := e.cancels[j.ID]; j.State.Finished() && !active {
			finished = append(finished, j)
		}
	}
	if len(finished) <= maxFinished {
		return
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].Created.After(finished[b].Created) })
	for _, j := range finished[maxFinished:] {
		delete(e.jobs, j.ID)
	}
}

// reset clears a job's progress so it can run again.
func (j *Job) reset() {
	j.State, j.Error, j.Problem, j.FilePath = Queued, "", nil, ""
	j.Part, j.Percent, j.Downloaded, j.Total, j.Speed, j.ETA, j.Files = 0, 0, 0, 0, 0, 0, 0
}

// queueChanged asks for the queue to be saved soon.
func (e *Engine) queueChanged() {
	select {
	case e.saveReq <- struct{}{}:
	default: // a save is already due
	}
}

// saver writes the queue at most once a second; changes made meanwhile wait
// in saveReq and go out in the next write.
func (e *Engine) saver() {
	for range e.saveReq {
		e.saveQueue()
		time.Sleep(time.Second)
	}
}

func (e *Engine) saveQueue() {
	// saveMu covers the snapshot as well as the write, so writes land in the
	// order their snapshots were taken.
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	e.mu.Lock()
	if e.queuePath == "" || e.closing {
		e.mu.Unlock()
		return
	}
	e.pruneFinished()
	saved := make([]savedJob, 0, len(e.jobs))
	for _, j := range e.jobs {
		saved = append(saved, savedJob{Job: *j, Pad: j.pad, Photos: j.photos})
	}
	path := e.queuePath
	e.mu.Unlock()

	sort.Slice(saved, func(a, b int) bool { return saved[a].Created.Before(saved[b].Created) })
	data, err := json.MarshalIndent(saved, "", "  ")
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		slog.Warn("saving queue", "path", path, "err", err)
	}
}
