// Package paths resolves where the loader and the game keep their files.
//
// The game is sm64coopdx. Its user data directory is the same one the game
// itself uses, so mods, palettes and saves are shared with it:
//
//	Windows  %APPDATA%\sm64coopdx
//	macOS    ~/Library/Application Support/sm64coopdx
//	Linux    $XDG_DATA_HOME/sm64coopdx or ~/.local/share/sm64coopdx
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// AppName is the directory name used for the loader's own settings.
const AppName = "coop-loader"

// LoaderData returns (and creates) the loader's configuration directory.
func LoaderData() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, AppName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// GameData returns the sm64coopdx user-data directory.
func GameData() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "sm64coopdx"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "sm64coopdx"), nil
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "sm64coopdx"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "sm64coopdx"), nil
	}
}

// ExeName is the game executable name for the current platform.
func ExeName() string {
	if runtime.GOOS == "windows" {
		return "sm64coopdx.exe"
	}
	return "sm64coopdx"
}
