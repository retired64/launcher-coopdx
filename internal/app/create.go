package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/retired64/coop-loader/internal/devicecopy"
	"github.com/retired64/coop-loader/internal/fsutil"
)

// CreateMod makes a small, safe starter mod in the user's mods folder. It does
// not enable it or touch the game's configuration.
func (s *Service) CreateMod(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return "", ErrGameRunning
	}
	clean, err := safeModName(name)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.pathsLocked().UserMods(), clean)
	if fsutil.Exists(dir) {
		return "", fmt.Errorf("ya existe un mod llamado %q", clean)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	body := fmt.Sprintf(`-- name: %s
-- description: Un mod nuevo hecho con Coop Loader.
-- version: 1.0
-- author: %s

-- Escribe aquí tu mod. El juego lo carga desde main.lua.
-- Abre la consola del juego y usa /reload después de guardar.

print("[%s] cargado")
`, clean, os.Getenv("USER"), clean)
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, "main.lua"), []byte(body), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return clean, nil
}

func safeModName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("ponle un nombre al mod")
	}
	if len([]rune(name)) > 60 {
		return "", errors.New("el nombre es demasiado largo")
	}
	if !devicecopy.SafeComponent(name) {
		return "", errors.New("ese nombre no se puede usar")
	}
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return "", errors.New("el nombre tiene un carácter que no se puede usar")
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", errors.New("el nombre no puede terminar en punto ni espacio")
	}
	return name, nil
}
