package app

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/retired64/coop-loader/internal/game"
	"github.com/retired64/coop-loader/internal/rom"
	"github.com/retired64/coop-loader/internal/settings"
)

// ROMStatus is the state of the game's borrowed look.
type ROMStatus struct {
	Ready    bool   `json:"ready"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Path     string `json:"path,omitempty"`
	ExtraHUD bool   `json:"extraHud"`
	Busy     bool   `json:"busy,omitempty"`
}

// ROMDir is where the extracted pictures and fonts live.
func (s *Service) ROMDir() string { return rom.CacheDir(s.loaderDir) }

// ROMStatus reports the current state and starts extraction in the background
// the first time it is asked. It never blocks the HTTP handler.
func (s *Service) ROMStatus() ROMStatus {
	s.romMu.Lock()
	if s.romReady {
		st := s.romStatus
		s.romMu.Unlock()
		return st
	}
	if s.romBusy {
		s.romMu.Unlock()
		return ROMStatus{Reason: "extracting", Busy: true, Message: "Leyendo tu ROM…"}
	}
	if s.romChecked {
		st := s.romStatus
		s.romMu.Unlock()
		return st
	}
	s.romBusy = true
	s.romMu.Unlock()

	go s.ensureROM()
	return ROMStatus{Reason: "extracting", Busy: true, Message: "Leyendo tu ROM…"}
}

// SetROMPath validates a ROM and rebuilds the cache. It is synchronous because
// the user is waiting for the result.
func (s *Service) SetROMPath(path string) (ROMStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ROMStatus{}, fmt.Errorf("no se pudo abrir la ROM: %w", err)
	}
	st, err := s.extractROM(data, path)
	if err != nil {
		return ROMStatus{}, err
	}
	if err := s.saveROMPath(path); err != nil {
		return st, err
	}
	return st, nil
}

// UseROMBytes extracts a ROM the browser sent. The file itself is not kept; the
// cache is enough to keep the look between restarts.
func (s *Service) UseROMBytes(data []byte, name string) (ROMStatus, error) {
	st, err := s.extractROM(data, name)
	if err != nil {
		return ROMStatus{}, err
	}
	_ = s.saveROMPath("")
	return st, nil
}

func (s *Service) saveROMPath(path string) error {
	v, err := s.settings.Load()
	if err != nil {
		return err
	}
	v.ROMPath = path
	return s.settings.Save(v)
}

func (s *Service) ensureROM() {
	defer func() {
		s.romMu.Lock()
		s.romBusy = false
		s.romChecked = true
		s.romMu.Unlock()
	}()

	v, err := s.settings.Load()
	if err != nil {
		v = settings.Default()
	}
	gamePath := s.gamePathLocked()
	exe := ""
	if gamePath != "" {
		exe = filepath.Join(gamePath, game.Exe())
		if _, err := os.Stat(exe); err != nil {
			exe = ""
		}
	}
	stamp := rom.EXEStamp(exe)

	cached := func() (rom.Version, bool) {
		cache, err := rom.ReadCache(s.loaderDir)
		if err != nil || cache.Version != rom.AssetVersion {
			return rom.Version{}, false
		}
		if _, err := os.Stat(filepath.Join(rom.CacheDir(s.loaderDir), "dialog.ttf")); err != nil {
			return rom.Version{}, false
		}
		return cache, true
	}

	path, r, findErr := rom.Find(v.ROMPath, s.gameData, gamePath)
	if findErr != nil {
		if cache, ok := cached(); ok {
			s.setROMStatus(ROMStatus{
				Ready:   true,
				Path:    cache.ROMPath,
				Message: "Estilo del juego cargado.",
			})
			return
		}
		s.setROMStatus(ROMStatus{
			Reason:  "not-found",
			Message: rom.DescribeError(findErr),
		})
		s.logf("rom: %s", rom.DescribeError(findErr))
		return
	}

	if cache, ok := cached(); ok && cache.ROM == r.SHA1() && (stamp == "" || cache.EXE == "" || cache.EXE == stamp) {
		if rom.CacheUsable(s.loaderDir, r.SHA1(), stamp) {
			s.setROMStatus(ROMStatus{
				Ready:   true,
				Path:    path,
				Message: "Estilo del juego cargado.",
			})
			return
		}
	}

	start := time.Now()
	assets, err := rom.Extract(r.Data, path, exe, rom.CacheDir(s.loaderDir))
	if err != nil {
		s.setROMStatus(ROMStatus{Reason: "error", Message: rom.DescribeError(err)})
		s.logf("rom: %s", err)
		return
	}
	s.logf("rom: %d textures, %d letters and two fonts in %d ms", assets.Textures, assets.HUD, time.Since(start).Milliseconds())
	s.setROMStatus(ROMStatus{
		Ready:    true,
		Path:     path,
		ExtraHUD: assets.ExtraHUD,
		Message:  "Estilo del juego cargado.",
	})
}

func (s *Service) extractROM(data []byte, name string) (ROMStatus, error) {
	s.romMu.Lock()
	s.romReady = false
	s.romChecked = false
	s.romMu.Unlock()
	gamePath := s.gamePathLocked()
	exe := ""
	if gamePath != "" {
		exe = filepath.Join(gamePath, game.Exe())
		if _, err := os.Stat(exe); err != nil {
			exe = ""
		}
	}
	start := time.Now()
	assets, err := rom.Extract(data, name, exe, rom.CacheDir(s.loaderDir))
	if err != nil {
		s.romMu.Lock()
		s.romChecked = true
		s.romStatus = ROMStatus{Reason: "error", Message: rom.DescribeError(err)}
		s.romMu.Unlock()
		return ROMStatus{}, err
	}
	st := ROMStatus{
		Ready:    true,
		Path:     name,
		ExtraHUD: assets.ExtraHUD,
		Message:  "Estilo del juego cargado.",
	}
	s.setROMStatus(st)
	s.logf("rom: %d textures, %d letters and two fonts in %d ms", assets.Textures, assets.HUD, time.Since(start).Milliseconds())
	return st, nil
}

func (s *Service) setROMStatus(st ROMStatus) {
	s.romMu.Lock()
	s.romStatus = st
	s.romReady = st.Ready
	s.romMu.Unlock()
}

func (s *Service) logf(format string, args ...any) {
	fmt.Printf("[app] "+format+"\n", args...)
}
