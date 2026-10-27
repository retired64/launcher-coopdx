package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// testHub is a tiny relay that broadcasts every announce to the other
// websockets in the same room path. Real trackers do peer matching; for a
// loopback test this is enough and it keeps the public trackers out.
type testHub struct {
	mu    sync.Mutex
	rooms map[string]map[*hubConn]bool
}

type hubConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func newTestHub(t *testing.T) (*testHub, *httptest.Server) {
	t.Helper()
	h := &testHub{rooms: map[string]map[*hubConn]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	return h, srv
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

		h.mu.Lock()
		peers := make([]*hubConn, 0, len(h.rooms[room]))
		for other := range h.rooms[room] {
			if other != hc {
				peers = append(peers, other)
			}
		}
		h.mu.Unlock()
		// Real trackers split an announce into one message per offer. Do the
		// same so the engine sees the `offer` (singular) field.
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

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHostJoinEndToEnd(t *testing.T) {
	_, hub := newTestHub(t)
	const code = "K7QD-9XPA"

	hostGame, hostPort := fakeGame(t)
	joinGame, _ := fakeGame(t)

	var mu sync.Mutex
	logs := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		t.Logf(format, args...)
	}

	host := New(Config{
		Mode:            ModeHost,
		Code:            code,
		RelayURL:        hub.URL,
		DisableTrackers: true,
		KDFIterations:   1000,
		GamePort:        hostPort,
		Logf:            logs,
	})
	join := New(Config{
		Mode:            ModeJoin,
		Code:            code,
		RelayURL:        hub.URL,
		DisableTrackers: true,
		KDFIterations:   1000,
		DebugPackets:    true,
		Logf:            logs,
	})

	if _, err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := join.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		join.Stop()
		host.Stop()
	})

	waitFor(t, "host waiting", 5*time.Second, func() bool {
		st := host.State()
		return st.Phase == PhaseWaiting && st.Trackers > 0
	})
	waitFor(t, "both connected", 25*time.Second, func() bool {
		hs, js := host.State(), join.State()
		return hs.Phase == PhaseConnected && len(hs.Friends) == 1 &&
			js.Phase == PhaseConnected && len(js.Friends) == 1
	})
	waitFor(t, "both routes", 5*time.Second, func() bool {
		hs, js := host.State(), join.State()
		return hs.Friends[0].Route != nil && js.Friends[0].Route != nil
	})

	hs := host.State()
	js := join.State()
	if hs.Friends[0].Route == nil || *hs.Friends[0].Route != "direct" {
		t.Fatalf("host route = %v", hs.Friends[0].Route)
	}
	if js.Friends[0].Route == nil || *js.Friends[0].Route != "direct" {
		t.Fatalf("join route = %v", js.Friends[0].Route)
	}
	if hs.Port != hostPort || js.Port == 0 {
		t.Fatalf("ports: host=%d join=%d", hs.Port, js.Port)
	}

	// Give SCTP a moment after the channel reports open; a game datagram sent
	// in the same millisecond can be dropped because the game channel is
	// intentionally unreliable.
	time.Sleep(750 * time.Millisecond)

	friendID := hs.Friends[0].ID
	peerPort := host.bridge.PeerPort(friendID)
	if peerPort == 0 {
		t.Fatal("host has no private port for the friend")
	}

	// Real game datagrams are zlib streams, whose first byte is 0x78 = 120,
	// which is the protocol's "this is a game packet" marker.
	boot := deflate(t, []byte("boot"))
	if _, err := joinGame.WriteToUDP(boot, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: js.Port}); err != nil {
		t.Fatal(err)
	}
	if got := inflate(t, readUDP(t, hostGame, 5*time.Second)); string(got) != "boot" {
		t.Fatalf("host game got %q", got)
	}

	// Host -> join.
	toJoin := deflate(t, []byte("to join"))
	if _, err := hostGame.WriteToUDP(toJoin, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: peerPort}); err != nil {
		t.Fatal(err)
	}
	if got := inflate(t, readUDP(t, joinGame, 5*time.Second)); string(got) != "to join" {
		t.Fatalf("join game got %q", got)
	}

	// Join -> host.
	toHost := deflate(t, []byte("to host"))
	if _, err := joinGame.WriteToUDP(toHost, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: js.Port}); err != nil {
		t.Fatal(err)
	}
	if got := inflate(t, readUDP(t, hostGame, 5*time.Second)); string(got) != "to host" {
		t.Fatalf("host game got %q", got)
	}

	host.SetGameIn(true)
	if !host.State().GameIn {
		t.Fatal("SetGameIn did not stick")
	}

	join.Stop()
	waitFor(t, "join stopped", 5*time.Second, func() bool {
		return join.State().Phase == PhaseStopped
	})
}

func TestJoinRejectsAnotherCode(t *testing.T) {
	room, err := DeriveWith("ABCD-1234", 1000)
	if err != nil {
		t.Fatal(err)
	}
	other, err := DeriveWith("ZZZZ-ZZZZ", 1000)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := room.Seal("v=0")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Open(sealed); ok {
		t.Fatal("another room opened the SDP")
	}
}

func TestSDPAttributes(t *testing.T) {
	sdp := "v=0\r\na=ice-ufrag:abcd\r\na=fingerprint:sha-256 AA:BB:CC\r\n"
	if got := sdpAttribute(sdp, "ice-ufrag"); got != "abcd" {
		t.Fatalf("ufrag = %q", got)
	}
	if got := sdpAttribute(sdp, "fingerprint"); got != "sha-256 AA:BB:CC" {
		t.Fatalf("fingerprint = %q", got)
	}
	if got := sdpAttribute(sdp, "nope"); got != "" {
		t.Fatalf("missing = %q", got)
	}
}

func TestSignalMessageShape(t *testing.T) {
	room, err := DeriveWith("ABCD-1234", 1000)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_, data, err := conn.Read(r.Context())
		if err == nil {
			select {
			case got <- string(data):
			default:
			}
		}
		<-r.Context().Done()
	}))
	defer hub.Close()

	sig := NewSignal(room, "ABCDEFGHIJKLMNOPQRST", hub.URL, nil, SignalEvents{})
	sig.Start(context.Background())
	defer sig.Close()

	var text string
	select {
	case text = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no announce reached the relay")
	}
	for _, want := range []string{`"action":"announce"`, `"numwant":10`, `"offers":[]`, `"left":1`} {
		if !strings.Contains(text, want) {
			t.Errorf("announce lacks %s: %s", want, text)
		}
	}
	// info_hash must be 20 escaped bytes, not base64 and not 40 hex chars.
	if strings.Count(text, `\u00`) < 20 {
		t.Errorf("info_hash is not a binary string: %s", text)
	}
}

func TestGameExitedClearsGameIn(t *testing.T) {
	e := New(Config{
		Mode:            ModeHost,
		Code:            "ABCD-1234",
		DisableRelay:    true,
		DisableTrackers: true,
		KDFIterations:   1000,
		GamePort:        12345,
	})
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()

	e.SetGameIn(true)
	if !e.State().GameIn {
		t.Fatal("gameIn did not stick")
	}
	e.GameExited()
	if e.State().GameIn {
		t.Fatal("gameIn stayed true after the game exited")
	}
	e.SetGameIn(true)
	e.Stop()
	e.GameExited() // must be a no-op after Stop
}

func TestReconnectAfterLinkClose(t *testing.T) {
	_, hub := newTestHub(t)
	const code = "W4RX-2QPB"

	hostGame, hostPort := fakeGame(t)
	joinGame, _ := fakeGame(t)

	host := New(Config{
		Mode:            ModeHost,
		Code:            code,
		RelayURL:        hub.URL,
		DisableTrackers: true,
		KDFIterations:   1000,
		GamePort:        hostPort,
		Logf:            t.Logf,
	})
	join := New(Config{
		Mode:            ModeJoin,
		Code:            code,
		RelayURL:        hub.URL,
		DisableTrackers: true,
		KDFIterations:   1000,
		Logf:            t.Logf,
	})
	if _, err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := join.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		join.Stop()
		host.Stop()
	})

	waitFor(t, "connected", 25*time.Second, func() bool {
		hs, js := host.State(), join.State()
		return hs.Phase == PhaseConnected && len(hs.Friends) == 1 &&
			js.Phase == PhaseConnected && len(js.Friends) == 1
	})
	waitFor(t, "route", 5*time.Second, func() bool {
		return host.State().Friends[0].Route != nil && join.State().Friends[0].Route != nil
	})
	time.Sleep(500 * time.Millisecond)

	friendID := host.State().Friends[0].ID
	joinPort := join.Port()
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: joinPort}

	// Wake the join bridge and prove the path works before the cut.
	boot := deflate(t, []byte("before"))
	if _, err := joinGame.WriteToUDP(boot, addr); err != nil {
		t.Fatal(err)
	}
	if got := inflate(t, readUDP(t, hostGame, 5*time.Second)); string(got) != "before" {
		t.Fatalf("host got %q before the cut", got)
	}

	// Cut the join's PeerConnection from under the engine.
	join.mu.Lock()
	var pcs []*webrtc.PeerConnection
	for _, s := range join.slots {
		if s.link != nil {
			pcs = append(pcs, s.link.pc)
		}
		if s.spare != nil {
			pcs = append(pcs, s.spare.pc)
		}
	}
	join.mu.Unlock()
	if len(pcs) == 0 {
		t.Fatal("join had no link to cut")
	}
	for _, pc := range pcs {
		_ = pc.Close()
	}

	// The host must keep the player's place in the game.
	want := []byte{0x0f, 0x00, 0x00, 0x00, 0xff, 0x0f, 0xff, 0xff, 0xff}
	if got := inflate(t, readUDP(t, hostGame, 6*time.Second)); !bytes.Equal(got, want) {
		t.Fatalf("host did not receive STILL_HERE: %x", got)
	}

	// The join must build a new connection and come back.
	waitFor(t, "a new connection", 25*time.Second, func() bool {
		for _, line := range join.Logs() {
			if strings.Contains(line, "building a new connection") || strings.Contains(line, "asked for a new route") {
				return true
			}
		}
		return false
	})
	waitFor(t, "connected again", 20*time.Second, func() bool {
		js := join.State()
		return js.Phase == PhaseConnected && len(js.Friends) == 1 &&
			!js.Friends[0].Reconnecting && js.Friends[0].Route != nil
	})
	time.Sleep(500 * time.Millisecond)

	// And game traffic must flow again.
	again := deflate(t, []byte("after"))
	if _, err := joinGame.WriteToUDP(again, addr); err != nil {
		t.Fatal(err)
	}
	if got := inflate(t, readUDP(t, hostGame, 5*time.Second)); string(got) != "after" {
		t.Fatalf("host got %q after the reconnect", got)
	}
	_ = friendID
}
