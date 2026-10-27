// Package game finds, inspects and launches sm64coopdx.
package game

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/retired64/coop-loader/internal/fsutil"
	"github.com/retired64/coop-loader/internal/paths"
)

// ErrNotFound means the configured folder does not contain the game.
var ErrNotFound = errors.New("sm64coopdx was not found in that folder")

// Exe returns the executable name for this platform.
func Exe() string { return paths.ExeName() }

// Executable returns the path to the game binary inside dir.
// Besides a plain sm64coopdx binary it understands the macOS .app layout.
func Executable(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(dir, Exe()),
		filepath.Join(dir, "sm64coopdx.app", "Contents", "MacOS", "sm64coopdx"),
		filepath.Join(dir, "Contents", "MacOS", "sm64coopdx"),
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
	}
	return "", false
}

// Resolve returns the folder that actually contains the executable.
func Resolve(dir string) (string, bool) {
	exe, ok := Executable(dir)
	if !ok {
		return "", false
	}
	return filepath.Dir(exe), true
}

// Valid reports whether dir contains the game executable.
func Valid(dir string) bool {
	_, ok := Executable(dir)
	return ok
}

// Detect looks in the usual places for a sm64coopdx folder.
// It returns "" when nothing is found; callers should let the user pick.
func Detect() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dirs := []string{
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Descargas"),
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Games"),
		filepath.Join(home, "Applications"),
		home,
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if p := os.Getenv(env); p != "" {
				dirs = append(dirs, filepath.Join(p, "sm64coopdx"))
			}
		}
		for _, drive := range []string{"C", "D", "E", "F"} {
			root := drive + `:\`
			if fsutil.Exists(root) {
				dirs = append(dirs,
					filepath.Join(root, "sm64coopdx"),
					filepath.Join(root, "Games", "sm64coopdx"),
				)
			}
		}
	}
	for _, dir := range dirs {
		if resolved, ok := Resolve(dir); ok {
			return resolved
		}
		if resolved, ok := Resolve(filepath.Join(dir, "sm64coopdx")); ok {
			return resolved
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), "coop") {
				continue
			}
			candidate := filepath.Join(dir, e.Name())
			if resolved, ok := Resolve(candidate); ok {
				return resolved
			}
			if resolved, ok := Resolve(filepath.Join(candidate, "sm64coopdx")); ok {
				return resolved
			}
		}
	}
	return ""
}

// Running reports whether the game process is alive.
//
// Mutating mods, saves or the config while the game runs is unsafe: the game
// rewrites its files on exit and would undo the change.
func Running() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "windows":
		out, err := exec.CommandContext(ctx, "tasklist", "/FI", "IMAGENAME eq "+Exe(), "/NH").Output()
		if err != nil {
			return false
		}
		return strings.Contains(strings.ToLower(string(out)), strings.ToLower(Exe()))
	case "darwin":
		err := exec.CommandContext(ctx, "pgrep", "-x", strings.TrimSuffix(Exe(), ".exe")).Run()
		return err == nil
	default:
		return runningLinux()
	}
}

func runningLinux() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	want := Exe()
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != want {
			continue
		}
		// A process that already exited but has not been reaped is a zombie.
		// It still shows its old name in /proc, so it must not count as a
		// running game.
		if stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil && procZombie(stat) {
			continue
		}
		return true
	}
	return false
}

// Launch starts the game detached from the loader.
// It returns as soon as the process has started.
func Launch(dir string, args []string) error {
	return LaunchNotify(dir, args, nil)
}

// LaunchNotify is Launch with a callback that runs when the game exits.
//
// The callback runs on a background goroutine. The session engine uses it to
// mark the game as out and forget the join-side game address, so the game can
// be opened again without restarting the room.
func LaunchNotify(dir string, args []string, onExit func()) error {
	exe, ok := Executable(dir)
	if !ok {
		return ErrNotFound
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	// The loader must not keep the game attached to its own stdin/stdout.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child when it exits. Without this a short-lived process stays
	// as a zombie and /proc still reports its name as if the game were open.
	go func() {
		_ = cmd.Wait()
		if onExit != nil {
			onExit()
		}
	}()
	return nil
}

// procZombie reports whether a /proc/<pid>/stat line is a zombie. The comm
// field is in parentheses and may itself contain spaces or parentheses, so
// the state is the first non-space byte after the last ')'.
func procZombie(stat []byte) bool {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return false
	}
	for j := i + 1; j < len(stat); j++ {
		if stat[j] == ' ' {
			continue
		}
		return stat[j] == 'Z'
	}
	return false
}

// Version reads the first line of a version file when the game ships one.
// It is best effort and returns "" when unknown.
func Version(dir string) string {
	f, err := os.Open(filepath.Join(dir, "version.txt"))
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	return strings.TrimSpace(line)
}
