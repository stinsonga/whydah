package main

import (
	"os"
	"os/exec"
	"syscall"
)

const bundleID = "local.whydah" // CFBundleIdentifier in packaging/macos/Info.plist

// openedAsApp reports whether Finder (LaunchServices) started us as
// Whydah.app, rather than a shell or our own background copy.
func openedAsApp() bool {
	return os.Getenv("__CFBundleIdentifier") == bundleID && os.Getenv("WHYDAH_SERVE") == ""
}

// serveInBackground starts the app again, detached, to run the server.
// macOS only launches an app once and a reopen (e.g. double-clicking it again)
// reaches a Cocoa event loop we don't have, so the copy Finder starts exits
// right away and each later launch finds the server and reopens the UI.
func serveInBackground() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), "WHYDAH_SERVE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
