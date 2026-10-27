// Package server exposes the loader core to the local web UI.
//
// The server binds to loopback and demands a random per-run token on every API
// call, so a random website cannot drive the loader through the browser.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/retired64/coop-loader/internal/app"
	"github.com/retired64/coop-loader/internal/catalog"
	"github.com/retired64/coop-loader/internal/devicecopy"
	"github.com/retired64/coop-loader/internal/eeprom"
	"github.com/retired64/coop-loader/internal/game"
	"github.com/retired64/coop-loader/internal/mods"
	"github.com/retired64/coop-loader/internal/session"
)

//go:embed web
var webFS embed.FS

// Server wires the app service to HTTP.
type Server struct {
	svc   *app.Service
	token string
}

// New returns a server with a fresh random token.
func New(svc *app.Service) (*Server, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return &Server{svc: svc, token: hex.EncodeToString(buf)}, nil
}

// Token is the secret required by the API.
func (s *Server) Token() string { return s.token }

// Handler returns the HTTP tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.wrap(s.status))
	mux.HandleFunc("GET /api/mods", s.wrap(s.listMods))
	mux.HandleFunc("POST /api/mods/toggle", s.wrap(s.toggleMod))
	mux.HandleFunc("POST /api/mods/remove", s.wrap(s.removeMod))
	mux.HandleFunc("POST /api/mods/install", s.wrap(s.installMod))

	mux.HandleFunc("GET /api/catalog/categories", s.wrap(s.categories))
	mux.HandleFunc("GET /api/catalog/list", s.wrap(s.listCatalog))
	mux.HandleFunc("GET /api/catalog/detail", s.wrap(s.detailCatalog))
	mux.HandleFunc("POST /api/catalog/install", s.wrap(s.installFromSite))
	mux.HandleFunc("POST /api/catalog/clear-cache", s.wrap(s.clearCatalogCache))

	mux.HandleFunc("GET /api/saves", s.wrap(s.listSaves))
	mux.HandleFunc("POST /api/saves/copy", s.wrap(s.copySlot))
	mux.HandleFunc("POST /api/saves/clear", s.wrap(s.clearSlot))
	mux.HandleFunc("POST /api/saves/import", s.wrap(s.importSave))

	mux.HandleFunc("GET /api/profiles", s.wrap(s.listProfiles))
	mux.HandleFunc("POST /api/profiles/switch", s.wrap(s.switchProfile))
	mux.HandleFunc("POST /api/profiles/create", s.wrap(s.createProfile))
	mux.HandleFunc("POST /api/profiles/delete", s.wrap(s.deleteProfile))

	mux.HandleFunc("POST /api/settings/gamepath", s.wrap(s.setGamePath))
	mux.HandleFunc("POST /api/settings/closeonplay", s.wrap(s.setCloseOnPlay))
	mux.HandleFunc("POST /api/settings/netconsent", s.wrap(s.setNetConsent))

	mux.HandleFunc("GET /api/session/state", s.wrap(s.sessionState))
	mux.HandleFunc("POST /api/session/start", s.wrap(s.startSession))
	mux.HandleFunc("POST /api/session/stop", s.wrap(s.stopSession))
	mux.HandleFunc("POST /api/session/launch", s.wrap(s.launchSession))
	mux.HandleFunc("GET /api/session/logs", s.wrap(s.sessionLogs))

	mux.HandleFunc("GET /api/copy/state", s.wrap(s.copyState))
	mux.HandleFunc("POST /api/copy/start", s.wrap(s.startCopy))
	mux.HandleFunc("POST /api/copy/accept", s.wrap(s.acceptCopy))
	mux.HandleFunc("POST /api/copy/stop", s.wrap(s.stopCopy))
	mux.HandleFunc("GET /api/copy/logs", s.wrap(s.copyLogs))

	mux.HandleFunc("POST /api/open-external", s.wrap(s.openExternal))
	mux.HandleFunc("POST /api/game/detect", s.wrap(s.detectGame))
	mux.HandleFunc("POST /api/game/launch", s.wrap(s.launchGame))

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(static)))

	return s.headers(mux)
}

// --- handlers --------------------------------------------------------------

func (s *Server) status(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, s.svc.Status())
	return nil
}

func (s *Server) listMods(w http.ResponseWriter, r *http.Request) error {
	list := s.svc.LocalMods()
	if list == nil {
		list = []app.LocalMod{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mods": list})
	return nil
}

func (s *Server) toggleMod(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ID   string    `json:"id"`
		Kind mods.Kind `json:"kind"`
		On   bool      `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if in.Kind != mods.KindPack {
		in.Kind = mods.KindMod
	}
	disabled, err := s.svc.Toggle(in.ID, in.Kind, in.On)
	if err != nil {
		return err
	}
	if disabled == nil {
		disabled = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"disabled": disabled})
	return nil
}

func (s *Server) removeMod(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ID   string    `json:"id"`
		Kind mods.Kind `json:"kind"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if in.Kind != mods.KindPack {
		in.Kind = mods.KindMod
	}
	moved, err := s.svc.Remove(in.ID, in.Kind)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": moved})
	return nil
}

func (s *Server) installMod(w http.ResponseWriter, r *http.Request) error {
	allow := false
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return badRequest(err)
		}
		allow = r.FormValue("allowCompiled") == "true"
		file, header, err := r.FormFile("file")
		if err != nil {
			return badRequest(err)
		}
		defer file.Close()

		tmp, err := os.CreateTemp("", "coop-loader-*"+safeExt(header.Filename))
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		const maxUpload = 1 << 30
		n, err := io.Copy(tmp, io.LimitReader(file, maxUpload+1))
		if err != nil {
			tmp.Close()
			return err
		}
		if n > maxUpload {
			tmp.Close()
			return badRequest(errors.New("that file is bigger than 1 GiB"))
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		installed, err := s.svc.Install(tmpName, allow)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"installed": installed})
		return nil
	}

	var in struct {
		Path          string `json:"path"`
		AllowCompiled bool   `json:"allowCompiled"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	installed, err := s.svc.Install(in.Path, in.AllowCompiled)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"installed": installed})
	return nil
}

func (s *Server) categories(w http.ResponseWriter, r *http.Request) error {
	list, err := s.svc.Categories(r.Context())
	if err != nil {
		return httpError{status: http.StatusBadGateway, msg: err.Error()}
	}
	if list == nil {
		list = []catalog.Category{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": list})
	return nil
}

func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) error {
	q := catalog.Query{
		Sort:   r.URL.Query().Get("sort"),
		Search: r.URL.Query().Get("q"),
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 0 {
		q.Page = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("category")); err == nil && n > 0 {
		q.Category = n
	}
	listing, err := s.svc.Browse(r.Context(), q)
	if err != nil {
		return httpError{status: http.StatusBadGateway, msg: err.Error()}
	}
	if listing.Items == nil {
		listing.Items = []catalog.Item{}
	}
	writeJSON(w, http.StatusOK, listing)
	return nil
}

func (s *Server) detailCatalog(w http.ResponseWriter, r *http.Request) error {
	ref := r.URL.Query().Get("url")
	if ref == "" {
		return badRequest(errors.New("url is required"))
	}
	detail, err := s.svc.Detail(r.Context(), ref)
	if err != nil {
		return httpError{status: http.StatusBadGateway, msg: err.Error()}
	}
	writeJSON(w, http.StatusOK, detail)
	return nil
}

func (s *Server) installFromSite(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		URL           string `json:"url"`
		File          string `json:"file"`
		AllowCompiled bool   `json:"allowCompiled"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if strings.TrimSpace(in.URL) == "" {
		return badRequest(errors.New("url is required"))
	}
	out, err := s.svc.InstallFromSite(r.Context(), in.URL, in.File, in.AllowCompiled)
	if err != nil {
		if errors.Is(err, app.ErrGameRunning) || errors.Is(err, mods.ErrCompiled) ||
			errors.Is(err, mods.ErrTooBig) || errors.Is(err, mods.ErrNoMods) ||
			errors.Is(err, mods.ErrUnsupported) {
			return err
		}
		return httpError{status: http.StatusBadGateway, msg: err.Error()}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) clearCatalogCache(w http.ResponseWriter, r *http.Request) error {
	if err := s.svc.ClearCatalogCache(); err != nil {
		return httpError{status: http.StatusBadGateway, msg: err.Error()}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (s *Server) listSaves(w http.ResponseWriter, r *http.Request) error {
	state, err := s.svc.Saves()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, state)
	return nil
}

func (s *Server) copySlot(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		From int `json:"from"`
		To   int `json:"to"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if in.From < 0 || in.From >= eeprom.SlotCount || in.To < 0 || in.To >= eeprom.SlotCount {
		return badRequest(errors.New("slot out of range"))
	}
	return s.svc.CopySlot(in.From, in.To)
}

func (s *Server) clearSlot(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Slot int `json:"slot"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if in.Slot < 0 || in.Slot >= eeprom.SlotCount {
		return badRequest(errors.New("slot out of range"))
	}
	return s.svc.ClearSlot(in.Slot)
}

func (s *Server) importSave(w http.ResponseWriter, r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			return badRequest(err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			return badRequest(err)
		}
		defer file.Close()
		tmp, err := os.CreateTemp("", "coop-loader-save-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err := io.Copy(tmp, io.LimitReader(file, eeprom.FileSize+1)); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return s.svc.ImportSave(tmpName)
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	return s.svc.ImportSave(in.Path)
}

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) error {
	profiles, err := s.svc.Profiles()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active": profiles.Active,
		"names":  profiles.Names(),
		"sets":   profiles.Sets,
	})
	return nil
}

func (s *Server) switchProfile(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	return s.svc.SwitchProfile(in.Name)
}

func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	return s.svc.CreateProfile(in.Name)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	return s.svc.DeleteProfile(in.Name)
}

func (s *Server) setGamePath(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if err := s.svc.SetGamePath(in.Path); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.svc.Status())
	return nil
}

func (s *Server) setCloseOnPlay(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	return s.svc.SetCloseOnPlay(in.On)
}

func (s *Server) setNetConsent(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Accepted     bool `json:"accepted"`
		TermsVersion int  `json:"termsVersion"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if err := s.svc.SetNetConsent(in.Accepted, in.TermsVersion); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (s *Server) sessionState(w http.ResponseWriter, r *http.Request) error {
	state, ok := s.svc.SessionState()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"session": nil})
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": state})
	return nil
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Mode string `json:"mode"`
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	state, err := s.svc.StartSession(session.Mode(in.Mode), in.Code)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": state})
	return nil
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) error {
	s.svc.StopSession()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (s *Server) launchSession(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Slot    int `json:"slot"`
		Players int `json:"players"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	state, ok := s.svc.SessionState()
	if !ok {
		return app.ErrNoSession
	}
	state, err := s.svc.LaunchSession(state.Mode, in.Slot, in.Players)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": state})
	return nil
}

func (s *Server) sessionLogs(w http.ResponseWriter, r *http.Request) error {
	logs := s.svc.SessionLogs()
	if logs == nil {
		logs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
	return nil
}

func (s *Server) copyState(w http.ResponseWriter, r *http.Request) error {
	state, ok := s.svc.CopyState()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"copy": nil})
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"copy": state})
	return nil
}

func (s *Server) startCopy(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Role string `json:"role"`
		Code string `json:"code"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	state, err := s.svc.StartCopy(devicecopy.Role(in.Role), in.Code)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"copy": state})
	return nil
}

func (s *Server) acceptCopy(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Mods  bool `json:"mods"`
		Look  bool `json:"look"`
		Saves bool `json:"saves"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if err := s.svc.CopyAccept(devicecopy.Choice{Mods: in.Mods, Look: in.Look, Saves: in.Saves}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (s *Server) stopCopy(w http.ResponseWriter, r *http.Request) error {
	s.svc.StopCopy()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (s *Server) copyLogs(w http.ResponseWriter, r *http.Request) error {
	logs := s.svc.CopyLogs()
	if logs == nil {
		logs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
	return nil
}

func (s *Server) openExternal(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &in); err != nil {
		return badRequest(err)
	}
	if err := s.svc.OpenExternal(in.URL); err != nil {
		return badRequest(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

func (s *Server) detectGame(w http.ResponseWriter, r *http.Request) error {
	path := s.svc.DetectGame()
	writeJSON(w, http.StatusOK, map[string]any{"gamePath": path, "gameFound": path != ""})
	return nil
}

func (s *Server) launchGame(w http.ResponseWriter, r *http.Request) error {
	if err := s.svc.Launch(nil); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	return nil
}

// --- middleware ------------------------------------------------------------

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Security-Policy", "default-src 'none'")
		} else {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data: https://mods.sm64coopdx.com https://i.ytimg.com; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) wrap(h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := localHost(r); err != nil {
			writeError(w, http.StatusForbidden, "forbidden", err)
			return
		}
		if err := s.checkToken(r); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err)
			return
		}
		if err := h(w, r); err != nil {
			s.fail(w, err)
		}
	}
}

func (s *Server) checkToken(r *http.Request) error {
	got := r.Header.Get("X-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		return errors.New("bad or missing token")
	}
	return nil
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrGameRunning):
		writeError(w, http.StatusConflict, "game-running", err)
	case errors.Is(err, app.ErrNoConfig):
		writeError(w, http.StatusConflict, "no-config", err)
	case errors.Is(err, game.ErrNotFound):
		writeError(w, http.StatusBadRequest, "game-not-found", err)
	case errors.Is(err, app.ErrSessionInput):
		writeError(w, http.StatusBadRequest, "bad-session", err)
	case errors.Is(err, app.ErrNoSession):
		writeError(w, http.StatusConflict, "no-session", err)
	case errors.Is(err, app.ErrCopyInput):
		writeError(w, http.StatusBadRequest, "bad-copy", err)
	case errors.Is(err, app.ErrNoCopy):
		writeError(w, http.StatusConflict, "no-copy", err)
	case errors.Is(err, app.ErrCopyActive):
		writeError(w, http.StatusConflict, "copy-active", err)
	case errors.Is(err, app.ErrSessionBusy):
		writeError(w, http.StatusConflict, "session-active", err)
	case errors.Is(err, devicecopy.ErrNoReview):
		writeError(w, http.StatusConflict, "no-review", err)
	case errors.Is(err, mods.ErrCompiled):
		writeError(w, http.StatusConflict, "compiled", err)
	case errors.Is(err, mods.ErrTooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "too-big", err)
	case errors.Is(err, mods.ErrNoMods):
		writeError(w, http.StatusUnprocessableEntity, "no-mods", err)
	default:
		var he httpError
		if errors.As(err, &he) {
			writeError(w, he.status, "bad-request", err)
			return
		}
		writeError(w, http.StatusInternalServerError, "error", err)
	}
}

// --- helpers ---------------------------------------------------------------

func localHost(r *http.Request) error {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return nil
	default:
		return fmt.Errorf("unexpected host %q", host)
	}
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(err error) error { return httpError{status: http.StatusBadRequest, msg: err.Error()} }

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error(), "code": code})
}

func safeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".zip", ".lua", ".luac":
		return ext
	default:
		return ".zip"
	}
}
