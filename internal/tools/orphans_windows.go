package tools

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// On Windows, Whydah puts itself in a job object that kills everything in it
// when Whydah's handle to it closes, which Windows does when Whydah exits for
// any reason, crashes and End task included. Helpers start in the job, so
// none can outlive Whydah and race a resumed job for its files; programs
// meant to outlive it (StartOutside) break away from it.

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	createJobObject          = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	assignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitBreakawayOK         = 0x00000800
	jobObjectLimitKillOnJobClose      = 0x00002000
)

// jobLimits is JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
	IoCounters              [6]uint64
	ProcessMemoryLimit      uintptr
	JobMemoryLimit          uintptr
	PeakProcessMemoryUsed   uintptr
	PeakJobMemoryUsed       uintptr
}

var joinJob = sync.OnceFunc(func() {
	job, _, err := createJobObject.Call(0, 0)
	if job == 0 {
		slog.Warn("couldn't make a job object; helpers may outlive a crash", "err", err)
		return
	}
	// The handle is never closed: Windows closes it when Whydah exits.
	limits := jobLimits{LimitFlags: jobObjectLimitKillOnJobClose | jobObjectLimitBreakawayOK}
	if r, _, err := setInformationJobObject.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits)); r == 0 {
		slog.Warn("couldn't set up the job object; helpers may outlive a crash", "err", err)
		return
	}
	self, _ := syscall.GetCurrentProcess()
	if r, _, err := assignProcessToJobObject.Call(job, uintptr(self)); r == 0 {
		slog.Warn("couldn't join the job object; helpers may outlive a crash", "err", err)
	}
})

// reapOrphans deletes throwaway browser profiles left in the temp folder by a
// Whydah that crashed. Its helpers died with it (see joinJob).
func reapOrphans(context.Context, []string) {
	joinJob()
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), BrowserProfilePrefix+"*"))
	for _, dir := range dirs {
		go removeProfile(dir)
	}
}

// removeProfile deletes a leftover browser profile, unless a browser still
// has it open: Chrome keeps the profile's lockfile open while it runs, so
// it can't be deleted. A profile younger than a minute may be one that's
// still starting, so it's left alone.
func removeProfile(dir string) {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() || !strings.HasPrefix(filepath.Base(dir), BrowserProfilePrefix) || time.Since(st.ModTime()) < time.Minute {
		return
	}
	if err := os.Remove(filepath.Join(dir, "lockfile")); err != nil && !os.IsNotExist(err) {
		return
	}
	if err := os.RemoveAll(dir); err == nil {
		slog.Info("deleted a browser profile left from an earlier run", "dir", dir)
	}
}
