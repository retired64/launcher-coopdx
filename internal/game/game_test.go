package game

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestProcZombie(t *testing.T) {
	cases := []struct {
		stat string
		want bool
	}{
		{"4321 (sm64coopdx) Z 1 4321 4321 0 -1", true},
		{"4321 (sm64coopdx) R 1 4321 4321 0 -1", false},
		{"4321 (weird ) name) S 1 4321 4321 0 -1", false},
		{"4321 (weird ) name) Z 1 4321 4321 0 -1", true},
		{"broken", false},
	}
	for _, c := range cases {
		if got := procZombie([]byte(c.stat)); got != c.want {
			t.Errorf("procZombie(%q) = %v, want %v", c.stat, got, c.want)
		}
	}
}

func TestLaunchNotifyRunsOnExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, Exe())
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := LaunchNotify(dir, nil, func() { close(done) }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the exit callback never ran")
	}
}
