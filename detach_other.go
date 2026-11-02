//go:build !darwin

package main

func openedAsApp() bool { return false }

func serveInBackground() error { return nil }
