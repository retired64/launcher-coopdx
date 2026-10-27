package session

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func fakeGame(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, conn.LocalAddr().(*net.UDPAddr).Port
}

func readUDP(t *testing.T, conn *net.UDPConn, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 64<<10)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("reading UDP: %v", err)
	}
	return append([]byte(nil), buf[:n]...)
}

func inflate(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	return out
}

func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBridgeHostRoundTrip(t *testing.T) {
	game, gamePort := fakeGame(t)
	packets := make(chan []byte, 4)
	b := NewBridge(BridgeConfig{
		Mode:     ModeHost,
		GamePort: gamePort,
		OnPacket: func(peer string, data []byte) {
			if peer == "friend" {
				packets <- append([]byte(nil), data...)
			}
		},
	})
	if _, err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	peerPort, err := b.PeerOpen("friend")
	if err != nil {
		t.Fatal(err)
	}
	if peerPort == 0 {
		t.Fatal("no peer port")
	}

	// The game sends from 127.0.0.1:gamePort to the friend's private socket.
	toPeer := []byte("from the game")
	if _, err := game.WriteToUDP(toPeer, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: peerPort}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-packets:
		if !bytes.Equal(got, toPeer) {
			t.Fatalf("got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the bridge never saw the game packet")
	}

	// A remote packet goes to 7777 (here, the fake game port) from the same
	// private socket.
	remote := []byte("from the friend")
	if err := b.SendToGame("friend", remote); err != nil {
		t.Fatal(err)
	}
	if got := readUDP(t, game, 2*time.Second); !bytes.Equal(got, remote) {
		t.Fatalf("game got %q", got)
	}

	if err := b.SendToGame("friend", make([]byte, MaxGamePacket+1)); err == nil {
		t.Fatal("oversized packet should be refused")
	}
	b.PeerClose("friend")
	if _, err := b.PeerOpen("friend"); err != nil {
		t.Fatalf("reopening after close: %v", err)
	}
}

func TestBridgeJoinRoundTrip(t *testing.T) {
	game, _ := fakeGame(t)
	packets := make(chan []byte, 4)
	gameIn := make(chan struct{}, 1)
	b := NewBridge(BridgeConfig{
		Mode:     ModeJoin,
		GamePort: 1, // unused for join
		OnPacket: func(peer string, data []byte) {
			if peer == "host" {
				packets <- append([]byte(nil), data...)
			}
		},
		OnGameIn: func() { gameIn <- struct{}{} },
	})
	port, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// The first datagram fixes the game address and marks the game as in.
	toBridge := []byte("hello bridge")
	if _, err := game.WriteToUDP(toBridge, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-packets:
		if !bytes.Equal(got, toBridge) {
			t.Fatalf("got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the bridge never saw the game")
	}
	select {
	case <-gameIn:
	case <-time.After(time.Second):
		t.Fatal("OnGameIn was not called")
	}

	if err := b.SendToGame("host", []byte("to the game")); err != nil {
		t.Fatal(err)
	}
	if got := readUDP(t, game, 2*time.Second); !bytes.Equal(got, []byte("to the game")) {
		t.Fatalf("game got %q", got)
	}
}

func TestBridgeStillHere(t *testing.T) {
	want := []byte{0x0f, 0x00, 0x00, 0x00, 0xff, 0x0f, 0xff, 0xff, 0xff}

	game, gamePort := fakeGame(t)
	b := NewBridge(BridgeConfig{Mode: ModeHost, GamePort: gamePort})
	if _, err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.PeerOpen("friend"); err != nil {
		t.Fatal(err)
	}

	b.SetLink("friend", false)
	got := inflate(t, readUDP(t, game, 2*time.Second))
	if !bytes.Equal(got, want) {
		t.Fatalf("STILL_HERE payload = %x, want %x", got, want)
	}

	// A second copy arrives two seconds later.
	got = inflate(t, readUDP(t, game, 3*time.Second))
	if !bytes.Equal(got, want) {
		t.Fatalf("second STILL_HERE = %x", got)
	}
	b.SetLink("friend", true)

	// And stops after the link is back.
	_ = game.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
	buf := make([]byte, 4096)
	if _, _, err := game.ReadFromUDP(buf); err == nil {
		t.Fatal("STILL_HERE kept flowing after the link came back")
	}
}

func TestBridgeKeepsAndReplaysState(t *testing.T) {
	game, gamePort := fakeGame(t)
	living := make(chan []byte, 16)
	replay := make(chan []byte, 16)

	b := NewBridge(BridgeConfig{
		Mode:     ModeHost,
		GamePort: gamePort,
		OnPacket: func(peer string, data []byte) {
			living <- append([]byte(nil), data...)
		},
		SendToPeer: func(peer string, data []byte) {
			replay <- append([]byte(nil), data...)
		},
	})
	if _, err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	peerPort, err := b.PeerOpen("friend")
	if err != nil {
		t.Fatal(err)
	}

	b.SetLink("friend", false)
	_ = readUDP(t, game, 2*time.Second) // STILL_HERE

	var sent [][]byte
	for i := 1; i <= 3; i++ {
		plain := make([]byte, 12)
		plain[0] = CtrlGame
		binary.LittleEndian.PutUint16(plain[1:3], uint16(i))
		packet := deflate(t, plain)
		sent = append(sent, packet)
		if _, err := game.WriteToUDP(packet, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: peerPort}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-living:
		case <-time.After(2 * time.Second):
			t.Fatal("game packet was not forwarded")
		}
	}

	b.SetLink("friend", true)
	for i, want := range sent {
		select {
		case got := <-replay:
			if !bytes.Equal(got, want) {
				t.Fatalf("replay %d = %x, want %x", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("replay %d never arrived", i)
		}
	}
}

func TestBridgeResetGameFixesNewAddress(t *testing.T) {
	first, _ := fakeGame(t)
	gameIn := make(chan struct{}, 4)
	b := NewBridge(BridgeConfig{
		Mode:     ModeJoin,
		GamePort: 1,
		OnGameIn: func() { gameIn <- struct{}{} },
	})
	port, err := b.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	if _, err := first.WriteToUDP(deflate(t, []byte("one")), addr); err != nil {
		t.Fatal(err)
	}
	<-gameIn
	if err := b.SendToGame("host", []byte("reply")); err != nil {
		t.Fatal(err)
	}
	if got := readUDP(t, first, 2*time.Second); string(got) != "reply" {
		t.Fatalf("first game got %q", got)
	}

	// The game closes and opens again on another port.
	b.ResetGame()
	if err := b.SendToGame("host", []byte("nobody")); err == nil {
		t.Fatal("SendToGame should need a new game packet after ResetGame")
	}

	second, _ := fakeGame(t)
	if _, err := second.WriteToUDP(deflate(t, []byte("two")), addr); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gameIn:
	case <-time.After(2 * time.Second):
		t.Fatal("OnGameIn was not called again")
	}
	if err := b.SendToGame("host", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if got := readUDP(t, second, 2*time.Second); string(got) != "second" {
		t.Fatalf("second game got %q", got)
	}
}
