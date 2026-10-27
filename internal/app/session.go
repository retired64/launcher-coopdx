package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/retired64/coop-loader/internal/session"
	"github.com/retired64/coop-loader/internal/settings"
)

// ErrNoSession means the UI asked for something that needs an active room.
var ErrNoSession = errors.New("there is no session running")

// ErrSessionInput means the mode, code or launch options are not valid.
var ErrSessionInput = errors.New("that session setting is not valid")

// StartSession creates a room and starts the meeting points.
//
// Host with an empty code gets a fresh one. Join needs a valid code. An
// existing room is stopped first, matching the reference client's "a new
// session started" behaviour.
func (s *Service) StartSession(mode session.Mode, code string) (session.State, error) {
	if mode != session.ModeHost && mode != session.ModeJoin {
		return session.State{}, fmt.Errorf("%w: mode must be host or join", ErrSessionInput)
	}
	if mode == session.ModeHost && strings.TrimSpace(code) == "" {
		generated, err := session.Generate()
		if err != nil {
			return session.State{}, err
		}
		code = generated
	}
	normalized, err := session.Normalize(code)
	if err != nil {
		return session.State{}, fmt.Errorf("%w: %v", ErrSessionInput, err)
	}

	s.sessStartMu.Lock()
	defer s.sessStartMu.Unlock()

	s.sessMu.Lock()
	old := s.sess
	s.sess = nil
	s.sessMu.Unlock()
	if old != nil {
		old.Stop()
	}

	cfg := session.Config{
		Mode:     mode,
		Code:     normalized,
		GamePort: session.DefaultGamePort,
	}
	if relay := strings.TrimSpace(os.Getenv("COOP_RELAY_URL")); relay != "" {
		cfg.RelayURL = relay
	}
	if os.Getenv("COOP_NO_TRACKERS") == "1" {
		cfg.DisableTrackers = true
	}

	eng := session.New(cfg)
	state, err := eng.Start(context.Background())
	if err != nil {
		eng.Stop()
		return session.State{}, err
	}
	s.sessMu.Lock()
	s.sess = eng
	s.sessMu.Unlock()
	return state, nil
}

// StopSession ends the room, leaving the game itself running.
func (s *Service) StopSession() {
	s.sessStartMu.Lock()
	defer s.sessStartMu.Unlock()

	s.sessMu.Lock()
	eng := s.sess
	s.sess = nil
	s.sessMu.Unlock()
	if eng != nil {
		eng.Stop()
	}
}

// SessionState returns the current room, or ok=false when idle.
func (s *Service) SessionState() (session.State, bool) {
	s.sessMu.Lock()
	eng := s.sess
	s.sessMu.Unlock()
	if eng == nil {
		return session.State{}, false
	}
	return eng.State(), true
}

// SessionLogs returns the recent engine log, newest last.
func (s *Service) SessionLogs() []string {
	s.sessMu.Lock()
	eng := s.sess
	s.sessMu.Unlock()
	if eng == nil {
		return []string{}
	}
	return eng.Logs()
}

// LaunchSession opens the game already pointed at the room.
//
// Host: --server 7777 --playercount N --skip-intro.
// Join: --client 127.0.0.1 <bridge port> --skip-intro, where the port comes
// from the running bridge, never from the caller.
func (s *Service) LaunchSession(mode session.Mode, slot, players int) (session.State, error) {
	s.sessMu.Lock()
	eng := s.sess
	s.sessMu.Unlock()
	if eng == nil {
		return session.State{}, ErrNoSession
	}
	state := eng.State()
	if state.Mode != mode {
		return state, fmt.Errorf("%w: this room is %s", ErrSessionInput, state.Mode)
	}
	if s.running() {
		return state, ErrGameRunning
	}

	switch state.Mode {
	case session.ModeHost:
		if slot < 1 || slot > 4 {
			return state, fmt.Errorf("%w: save slot must be 1 to 4", ErrSessionInput)
		}
		switch players {
		case 2, 4, 8, 16:
		default:
			return state, fmt.Errorf("%w: players must be 2, 4, 8 or 16", ErrSessionInput)
		}
		// The reference client records the chosen save slot and the game
		// port in sm64config.txt before launching. The game reads them back
		// for its direct-connection host screen.
		if err := s.setHostConfig(slot); err != nil && !errors.Is(err, ErrNoConfig) {
			return state, err
		}
		// Mark the game as in before spawning it. The exit callback can run
		// immediately for a game that fails to start, and it must not race
		// with a later SetGameIn(true).
		eng.SetGameIn(true)
		if err := s.LaunchNotify([]string{
			"--server", fmt.Sprintf("%d", session.DefaultGamePort),
			"--playercount", fmt.Sprintf("%d", players),
			"--skip-intro",
		}, eng.GameExited); err != nil {
			eng.SetGameIn(false)
			return state, err
		}
	case session.ModeJoin:
		port := eng.Port()
		if port < 1024 || port > 65535 {
			return state, fmt.Errorf("%w: the local game port is not ready", ErrSessionInput)
		}
		if err := s.LaunchNotify([]string{
			"--client", "127.0.0.1", fmt.Sprintf("%d", port),
			"--skip-intro",
		}, eng.GameExited); err != nil {
			return state, err
		}
	}
	return eng.State(), nil
}

// setHostConfig records coop_host_save_slot and coop_host_port without
// disturbing any other line of sm64config.txt.
func (s *Service) setHostConfig(slot int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running() {
		return ErrGameRunning
	}
	doc, err := s.readConfigLocked()
	if err != nil {
		return err
	}
	doc.SetValue("coop_host_save_slot", strconv.Itoa(slot))
	doc.SetValue("coop_host_port", strconv.Itoa(session.DefaultGamePort))
	return s.writeConfigLocked(doc)
}

// NetConsent returns the stored online-play notice, if any.
func (s *Service) NetConsent() *settings.Consent {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.settings.Load()
	if err != nil {
		return nil
	}
	return v.NetConsent
}

// SetNetConsent stores the answer to the online-play notice.
func (s *Service) SetNetConsent(accepted bool, termsVersion int) error {
	if termsVersion < 1 || termsVersion > 1000 {
		return fmt.Errorf("%w: bad terms version", ErrSessionInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.settings.Load()
	if err != nil {
		return err
	}
	v.NetConsent = &settings.Consent{
		Accepted:     accepted,
		TermsVersion: termsVersion,
		At:           time.Now().UTC().Format(time.RFC3339),
	}
	return s.settings.Save(v)
}
