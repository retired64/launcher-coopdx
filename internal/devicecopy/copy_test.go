package devicecopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testHub is a tiny tracker: it broadcasts each announce to the other sockets
// in the same room, splitting an offers array into one message per offer the
// way real trackers do.
type testHub struct {
	mu    sync.Mutex
	rooms map[string]map[*hubConn]bool
}

type hubConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func newTestHub(t *testing.T) *httptest.Server {
	t.Helper()
	h := &testHub{rooms: map[string]map[*hubConn]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	return srv
}

func (h *testHub) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	hc := &hubConn{conn: conn}
	room := r.URL.Path
	h.mu.Lock()
	if h.rooms[room] == nil {
		h.rooms[room] = map[*hubConn]bool{}
	}
	h.rooms[room][hc] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.rooms[room], hc)
		h.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var msg map[string]any
		var frames [][]byte
		if json.Unmarshal(data, &msg) == nil {
			if offers, ok := msg["offers"].([]any); ok && len(offers) > 0 {
				for _, raw := range offers {
					entry, _ := raw.(map[string]any)
					out := map[string]any{
						"action":    "announce",
						"info_hash": msg["info_hash"],
						"peer_id":   msg["peer_id"],
						"offer":     entry["offer"],
						"offer_id":  entry["offer_id"],
					}
					if b, err := json.Marshal(out); err == nil {
						frames = append(frames, b)
					}
				}
			} else if _, ok := msg["answer"]; ok {
				frames = append(frames, data)
			}
		}
		h.mu.Lock()
		peers := make([]*hubConn, 0, len(h.rooms[room]))
		for other := range h.rooms[room] {
			if other != hc {
				peers = append(peers, other)
			}
		}
		h.mu.Unlock()
		for _, frame := range frames {
			for _, other := range peers {
				other.mu.Lock()
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				_ = other.conn.Write(ctx, websocket.MessageText, frame)
				cancel()
				other.mu.Unlock()
			}
		}
	}
}

func waitState(t *testing.T, e *Engine, want Phase, timeout time.Duration) State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := e.State()
		if st.Phase == want {
			return st
		}
		if st.Phase == PhaseFailed {
			msg := ""
			if st.Message != nil {
				msg = *st.Message
			}
			t.Fatalf("copy failed: %s", msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := e.State()
	t.Fatalf("timed out waiting for %s; state=%+v", want, st)
	return State{}
}

func TestCopyEndToEnd(t *testing.T) {
	hub := newTestHub(t)
	data := []byte("print('hello from the other device')\n")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	manifest := Manifest{
		V:         ProtocolVersion,
		Device:    DeviceInfo{Name: "sender", Kind: "linux"},
		SaveNames: []*string{nil, nil, nil, nil},
		Presets:   []Preset{},
		Savs:      []ModSave{},
		Mods: []Mod{{
			ID:       "TestMod",
			Type:     "mod",
			Location: "user",
			Name:     "Test mod",
			Enabled:  true,
			IsFolder: true,
			Files:    []File{{Rel: "main.lua", Size: int64(len(data)), SHA256: hash}},
		}},
	}
	text, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	staging := t.TempDir()
	var applied []byte
	send := New(Config{
		Role:            RoleSend,
		Code:            "K7QD-9XPA",
		Device:          DeviceInfo{Name: "sender", Kind: "linux"},
		RelayURL:        hub.URL,
		DisableTrackers: true,
		KDFIterations:   1000,
		StagingRoot:     staging,
		BuildManifest: func() (*Source, error) {
			return &Source{
				Text:  string(text),
				Files: []FileRef{{Size: int64(len(data))}},
				Read: func(index int, offset int64, length int) ([]byte, error) {
					if index != 0 {
						return nil, ErrFileChanged
					}
					start := int(offset)
					return data[start : start+length], nil
				},
			}, nil
		},
	})
	get := New(Config{
		Role:            RoleGet,
		Code:            "K7QD-9XPA",
		Device:          DeviceInfo{Name: "receiver", Kind: "linux"},
		RelayURL:        hub.URL,
		DisableTrackers: true,
		KDFIterations:   1000,
		StagingRoot:     staging,
		Review: func(m *Manifest, route string) (Review, error) {
			plan := BuildPlan(m, LocalState{}, m.SaveStars(), Choice{Mods: true, Look: true, Saves: true}, route)
			return plan.Review, nil
		},
		Plan: func(m *Manifest, choice Choice, route string) (*Plan, error) {
			return BuildPlan(m, LocalState{}, m.SaveStars(), choice, route), nil
		},
		Apply: func(m *Manifest, plan *Plan, stagingDir string) (string, error) {
			part := filepath.Join(stagingDir, "in", "0")
			applied, err = os.ReadFile(part)
			return "", err
		},
	})
	t.Cleanup(func() {
		get.Stop()
		send.Stop()
	})

	if _, err := send.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := get.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, get, PhaseReview, 25*time.Second)
	if err := get.Accept(Choice{Mods: true, Look: true, Saves: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, get, PhaseDone, 25*time.Second)
	waitState(t, send, PhaseDone, 25*time.Second)
	if string(applied) != string(data) {
		t.Fatalf("applied %q, want %q", applied, data)
	}
}

func TestManifestRejectsTraversal(t *testing.T) {
	m := Manifest{
		V:         1,
		Device:    DeviceInfo{Name: "x", Kind: "linux"},
		SaveNames: []*string{nil, nil, nil, nil},
		Mods: []Mod{{
			ID: "M", Type: "mod", Location: "user", Name: "M", IsFolder: true,
			Files: []File{{Rel: "../evil.lua", Size: 1, SHA256: strings64()}},
		}},
	}
	if err := m.Validate(); err == nil {
		t.Fatal("traversal path was accepted")
	}
}

func strings64() string {
	return "0000000000000000000000000000000000000000000000000000000000000000"
}

func TestFrameRoundTrip(t *testing.T) {
	frame := EncodeFrame(7, 1<<33+5, []byte("abc"))
	got, ok := DecodeFrame(frame)
	if !ok || got.File != 7 || got.Offset != 1<<33+5 || string(got.Data) != "abc" {
		t.Fatalf("frame = %+v, %v", got, ok)
	}
	if _, ok := DecodeFrame(frame[:5]); ok {
		t.Fatal("short frame accepted")
	}
}

func TestCopyLargeAndEmptyFiles(t *testing.T) {
	hub := newTestHub(t)
	big := make([]byte, 9*1024*1024+3)
	for i := range big {
		big[i] = byte(i * 31)
	}
	small := []byte{}
	h1 := sha256.Sum256(small)
	h2 := sha256.Sum256(big)
	manifest := Manifest{
		V:         ProtocolVersion,
		Device:    DeviceInfo{Name: "sender", Kind: "linux"},
		SaveNames: []*string{nil, nil, nil, nil},
		Presets:   []Preset{},
		Savs:      []ModSave{},
		Mods: []Mod{{
			ID: "Big", Type: "mod", Location: "user", Name: "Big", Enabled: true, IsFolder: true,
			Files: []File{
				{Rel: "empty.bin", Size: 0, SHA256: hex.EncodeToString(h1[:])},
				{Rel: "big.bin", Size: int64(len(big)), SHA256: hex.EncodeToString(h2[:])},
			},
		}},
	}
	text, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	var got [][]byte
	send := New(Config{
		Role: RoleSend, Code: "W4RX-2QPB", Device: DeviceInfo{Name: "sender", Kind: "linux"},
		RelayURL: hub.URL, DisableTrackers: true, KDFIterations: 1000, StagingRoot: staging,
		BuildManifest: func() (*Source, error) {
			return &Source{
				Text:  string(text),
				Files: []FileRef{{Size: 0}, {Size: int64(len(big))}},
				Read: func(index int, offset int64, length int) ([]byte, error) {
					data := big
					if index == 0 {
						data = small
					}
					return data[int(offset) : int(offset)+length], nil
				},
			}, nil
		},
	})
	get := New(Config{
		Role: RoleGet, Code: "W4RX-2QPB", Device: DeviceInfo{Name: "receiver", Kind: "linux"},
		RelayURL: hub.URL, DisableTrackers: true, KDFIterations: 1000, StagingRoot: staging,
		Review: func(m *Manifest, route string) (Review, error) {
			return BuildPlan(m, LocalState{}, m.SaveStars(), Choice{Mods: true}, route).Review, nil
		},
		Plan: func(m *Manifest, choice Choice, route string) (*Plan, error) {
			return BuildPlan(m, LocalState{}, m.SaveStars(), choice, route), nil
		},
		Apply: func(m *Manifest, plan *Plan, stagingDir string) (string, error) {
			got = nil
			for _, index := range plan.Want {
				data, err := os.ReadFile(filepath.Join(stagingDir, "in", string(rune('0'+index))))
				if err != nil {
					return "", err
				}
				got = append(got, data)
			}
			return "", nil
		},
	})
	t.Cleanup(func() { get.Stop(); send.Stop() })
	if _, err := send.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := get.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, get, PhaseReview, 25*time.Second)
	if err := get.Accept(Choice{Mods: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, get, PhaseDone, 30*time.Second)
	waitState(t, send, PhaseDone, 30*time.Second)
	if len(got) != 2 || len(got[0]) != 0 || len(got[1]) != len(big) {
		t.Fatalf("got lengths %d %d, want 0 %d", len(got[0]), len(got[1]), len(big))
	}
	if !bytes.Equal(got[1], big) {
		t.Fatal("large file changed")
	}
}
