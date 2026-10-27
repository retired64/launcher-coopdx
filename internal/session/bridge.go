package session

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Wire and bridge limits taken from the reference client.
const (
	DefaultGamePort = 7777
	MaxGamePacket   = 4096
	HoldWindow      = 60 * time.Second
	HoldEvery       = 2 * time.Second
	HoldCacheBytes  = 2 << 20
	HoldFlushBytes  = 32 << 10
	HoldFlushEvery  = 50 * time.Millisecond
)

// BridgeConfig wires the loopback UDP bridge to the rest of the engine.
type BridgeConfig struct {
	Mode       Mode
	GamePort   int    // 0 means 7777
	Loopback   string // 0 means 127.0.0.1
	OnPacket   func(peerID string, data []byte)
	SendToPeer func(peerID string, data []byte)
	OnGameIn   func()
	Logf       func(format string, args ...any)
}

// Bridge translates between the game's UDP sockets and the DataChannel.
//
// Host: the game listens on 7777. Each friend gets a dedicated socket bound
// to 127.0.0.1:0; packets from the friend are sent from that socket to 7777,
// so the game sees a distinct player address. Replies arriving on the socket
// are attributed to that friend.
//
// Join: one socket is bound to 127.0.0.1:0 and its port is given to the game
// with --client. The first datagram from the game fixes the destination, and
// everything is symmetric afterwards.
type Bridge struct {
	cfg BridgeConfig

	mu       sync.Mutex
	gamePort int
	mode     Mode
	port     int
	join     *net.UDPConn
	gameAddr *net.UDPAddr
	peers    map[string]*net.UDPConn
	holds    map[string]*holdState
	gameSeen bool
	closed   bool
}

type holdState struct {
	downAt time.Time
	kept   map[uint16][]byte
	order  []uint16
	bytes  int

	pending  [][]byte
	flushing bool

	stopDown  chan struct{}
	stopFlush chan struct{}
}

// NewBridge builds a stopped bridge.
func NewBridge(cfg BridgeConfig) *Bridge {
	if cfg.GamePort == 0 {
		cfg.GamePort = DefaultGamePort
	}
	if cfg.Loopback == "" {
		cfg.Loopback = "127.0.0.1"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Bridge{
		cfg:      cfg,
		mode:     cfg.Mode,
		gamePort: cfg.GamePort,
		peers:    map[string]*net.UDPConn{},
		holds:    map[string]*holdState{},
	}
}

// Start opens the sockets this mode needs. Host needs none yet; join binds its
// ephemeral game-facing port before the game starts.
func (b *Bridge) Start() (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, errors.New("bridge is closed")
	}
	b.mode = b.cfg.Mode
	gamePort := b.gamePort
	loopback := b.cfg.Loopback
	b.mu.Unlock()

	if b.mode == ModeHost {
		b.mu.Lock()
		b.gameAddr = &net.UDPAddr{IP: net.ParseIP(loopback), Port: gamePort}
		b.mu.Unlock()
		return gamePort, nil
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(loopback), Port: 0})
	if err != nil {
		return 0, fmt.Errorf("could not open the local game port: %w", err)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		conn.Close()
		return 0, errors.New("bridge is closed")
	}
	b.join = conn
	b.port = conn.LocalAddr().(*net.UDPAddr).Port
	b.mu.Unlock()

	go b.readJoin(conn)
	return b.port, nil
}

// Port is the local UDP port the game should connect to. Host returns 7777;
// join returns the ephemeral port passed as --client.
func (b *Bridge) Port() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mode == ModeHost {
		return b.gamePort
	}
	return b.port
}

// markGameSeen calls OnGameIn once, the first time a datagram from the game
// arrives. It also covers a game opened outside the loader.
func (b *Bridge) markGameSeen() {
	b.mu.Lock()
	first := !b.gameSeen
	b.gameSeen = true
	b.mu.Unlock()
	if first && b.cfg.OnGameIn != nil {
		b.cfg.OnGameIn()
	}
}

// ResetGame forgets the join-side game address and the "game seen" flag. A
// restarted game may bind a different local port, so the next datagram must
// fix the address again.
func (b *Bridge) ResetGame() {
	b.mu.Lock()
	if b.mode == ModeJoin {
		b.gameAddr = nil
	}
	b.gameSeen = false
	b.mu.Unlock()
}

func (b *Bridge) readJoin(conn *net.UDPConn) {
	buf := make([]byte, 64<<10)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !src.IP.IsLoopback() {
			continue
		}
		b.mu.Lock()
		if b.closed || b.join != conn {
			b.mu.Unlock()
			return
		}
		b.gameAddr = &net.UDPAddr{IP: append(net.IP(nil), src.IP...), Port: src.Port}
		b.mu.Unlock()

		b.markGameSeen()
		b.fromGame("host", append([]byte(nil), buf[:n]...))
	}
}

// PeerOpen creates the private game-facing socket for one friend. It is only
// meaningful on the host.
func (b *Bridge) PeerOpen(peerID string) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, errors.New("bridge is closed")
	}
	if b.mode != ModeHost {
		b.mu.Unlock()
		return 0, errors.New("only the host opens one port per friend")
	}
	if _, ok := b.peers[peerID]; ok {
		port := b.gamePort
		b.mu.Unlock()
		return port, nil
	}
	loopback := net.ParseIP(b.cfg.Loopback)
	gamePort := b.gamePort
	b.mu.Unlock()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: 0})
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		conn.Close()
		return 0, errors.New("bridge is closed")
	}
	if old := b.peers[peerID]; old != nil {
		b.mu.Unlock()
		conn.Close()
		return old.LocalAddr().(*net.UDPAddr).Port, nil
	}
	b.peers[peerID] = conn
	port := conn.LocalAddr().(*net.UDPAddr).Port
	b.mu.Unlock()

	go b.readPeer(peerID, conn, gamePort, loopback)
	return port, nil
}

func (b *Bridge) readPeer(peerID string, conn *net.UDPConn, gamePort int, loopback net.IP) {
	buf := make([]byte, 64<<10)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !src.IP.Equal(loopback) || src.Port != gamePort {
			continue
		}
		b.markGameSeen()
		b.fromGame(peerID, append([]byte(nil), buf[:n]...))
	}
}

// PeerClose closes one friend's socket and forgets anything held for them.
func (b *Bridge) PeerClose(peerID string) {
	b.mu.Lock()
	conn := b.peers[peerID]
	delete(b.peers, peerID)
	h := b.holds[peerID]
	delete(b.holds, peerID)
	b.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	stopHold(h)
}

// fromGame forwards a datagram from the game and remembers it while the link
// is down.
func (b *Bridge) fromGame(peerID string, data []byte) {
	if len(data) == 0 {
		return
	}
	b.mu.Lock()
	hold := b.holds[peerID]
	down := hold != nil && !hold.downAt.IsZero()
	b.mu.Unlock()

	if down {
		b.remember(peerID, data)
	}
	if b.cfg.OnPacket != nil {
		b.cfg.OnPacket(peerID, data)
	}
}

// SendToGame delivers one remote datagram to the local game.
func (b *Bridge) SendToGame(peerID string, data []byte) error {
	if len(data) > MaxGamePacket {
		return fmt.Errorf("game packet is %d bytes, over the %d byte limit", len(data), MaxGamePacket)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("bridge is closed")
	}
	var conn *net.UDPConn
	var addr *net.UDPAddr
	if b.mode == ModeHost {
		conn = b.peers[peerID]
		if conn == nil {
			return errors.New("no game port for that friend")
		}
		addr = &net.UDPAddr{IP: net.ParseIP(b.cfg.Loopback), Port: b.gamePort}
	} else {
		conn = b.join
		addr = b.gameAddr
		if conn == nil || addr == nil {
			return errors.New("the game has not sent anything yet")
		}
	}
	_, err := conn.WriteToUDP(data, addr)
	return err
}

// SetLink starts or ends the "hold their place" behaviour for one friend.
//
// While down the game keeps receiving a tiny compressed STILL_HERE packet
// every two seconds for up to a minute, and state packets are kept so they can
// be replayed when the link returns.
func (b *Bridge) SetLink(peerID string, up bool) {
	if up {
		b.linkBack(peerID)
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	h := b.holds[peerID]
	if h == nil {
		h = &holdState{kept: map[uint16][]byte{}}
		b.holds[peerID] = h
	}
	if !h.downAt.IsZero() {
		b.mu.Unlock()
		return
	}
	h.downAt = time.Now()
	h.stopDown = make(chan struct{})
	stop := h.stopDown
	b.mu.Unlock()

	b.sendStillHere(peerID)
	b.cfg.Logf("%s: link down, holding their place in the game", short(peerID))
	go b.holdLoop(peerID, stop)
}

func (b *Bridge) holdLoop(peerID string, stop chan struct{}) {
	t := time.NewTicker(HoldEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		b.mu.Lock()
		h := b.holds[peerID]
		if h == nil || h.downAt.IsZero() {
			b.mu.Unlock()
			return
		}
		elapsed := time.Since(h.downAt)
		b.mu.Unlock()
		if elapsed > HoldWindow {
			b.cfg.Logf("%s: stopped holding their place after %s", short(peerID), HoldWindow)
			return
		}
		b.sendStillHere(peerID)
	}
}

func (b *Bridge) sendStillHere(peerID string) {
	_ = b.SendToGame(peerID, stillHerePacket())
}

func (b *Bridge) linkBack(peerID string) {
	b.mu.Lock()
	h := b.holds[peerID]
	if h == nil || h.downAt.IsZero() {
		b.mu.Unlock()
		return
	}
	elapsed := time.Since(h.downAt)
	stop := h.stopDown
	h.downAt = time.Time{}
	if stop != nil {
		close(stop)
	}
	h.stopDown = nil

	// Keep the order the game sent them in so the game's state advances in
	// the same sequence after a short cut.
	for _, key := range h.order {
		if data, ok := h.kept[key]; ok {
			h.pending = append(h.pending, data)
		}
	}
	count := len(h.pending)
	h.kept = map[uint16][]byte{}
	h.order = nil
	h.bytes = 0

	if len(h.pending) == 0 {
		b.mu.Unlock()
		b.cfg.Logf("%s: link back after %.1f s, nothing kept", short(peerID), elapsed.Seconds())
		return
	}
	if h.flushing {
		b.mu.Unlock()
		return
	}
	h.flushing = true
	h.stopFlush = make(chan struct{})
	b.mu.Unlock()

	b.cfg.Logf("%s: link back after %.1f s, sending %d kept packets", short(peerID), elapsed.Seconds(), count)
	b.flush(peerID)
}

func (b *Bridge) flush(peerID string) {
	t := time.NewTicker(HoldFlushEvery)
	defer t.Stop()
	for {
		batch := b.takePending(peerID, HoldFlushBytes)
		if len(batch) == 0 {
			return
		}
		for _, data := range batch {
			if b.cfg.SendToPeer != nil {
				b.cfg.SendToPeer(peerID, data)
			}
		}
		select {
		case <-t.C:
		}
	}
}

func (b *Bridge) takePending(peerID string, limit int) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.holds[peerID]
	if h == nil || h.flushing == false {
		return nil
	}
	if !h.downAt.IsZero() {
		// It went down again mid-flush; leave the rest queued.
		h.flushing = false
		return nil
	}
	var out [][]byte
	total := 0
	for len(h.pending) > 0 && total < limit {
		data := h.pending[0]
		h.pending = h.pending[1:]
		out = append(out, data)
		total += len(data)
	}
	if len(h.pending) == 0 {
		h.flushing = false
	}
	return out
}

func (b *Bridge) remember(peerID string, data []byte) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return
	}
	plain, err := io.ReadAll(io.LimitReader(zr, MaxGamePacket+1))
	_ = zr.Close()
	if err != nil || len(plain) < 3 || len(plain) > MaxGamePacket {
		return
	}
	key := binary.LittleEndian.Uint16(plain[1:3])
	if key == 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.holds[peerID]
	if h == nil || h.downAt.IsZero() {
		return
	}
	if old, ok := h.kept[key]; ok {
		h.bytes -= len(old)
	} else {
		h.order = append(h.order, key)
	}
	copied := append([]byte(nil), data...)
	h.kept[key] = copied
	h.bytes += len(copied)

	for h.bytes > HoldCacheBytes && len(h.order) > 0 {
		oldest := h.order[0]
		h.order = h.order[1:]
		if old, ok := h.kept[oldest]; ok {
			delete(h.kept, oldest)
			h.bytes -= len(old)
		}
	}
}

// Close closes every socket and stops all timers.
func (b *Bridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	join := b.join
	peers := b.peers
	holds := b.holds
	b.join = nil
	b.peers = map[string]*net.UDPConn{}
	b.holds = map[string]*holdState{}
	b.mu.Unlock()

	if join != nil {
		_ = join.Close()
	}
	for _, conn := range peers {
		_ = conn.Close()
	}
	for _, h := range holds {
		stopHold(h)
	}
}

func stopHold(h *holdState) {
	if h == nil {
		return
	}
	if h.stopDown != nil {
		close(h.stopDown)
		h.stopDown = nil
	}
	if h.stopFlush != nil {
		close(h.stopFlush)
		h.stopFlush = nil
	}
}

// PeerPort is the loopback port the game uses for one friend (host) or the
// single join port. Exported for diagnostics and tests.
func (b *Bridge) PeerPort(peerID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mode == ModeJoin {
		return b.port
	}
	if conn := b.peers[peerID]; conn != nil {
		return conn.LocalAddr().(*net.UDPAddr).Port
	}
	return 0
}

// stillHerePacket builds the exact 9-byte payload and deflates it.
//
//	0F 00 00 00 FF  checksum(~XOR32, little-endian)
//
// The XOR folds each byte at its position modulo four; with this fixed input
// it is always 0xFFFFFF0F.
func stillHerePacket() []byte {
	payload := []byte{0x0f, 0x00, 0x00, 0x00, 0xff}
	var x uint32
	for i, v := range payload {
		x ^= uint32(v) << (8 * (i % 4))
	}
	var sum [4]byte
	binary.LittleEndian.PutUint32(sum[:], ^x)
	payload = append(payload, sum[:]...)

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	return buf.Bytes()
}

func short(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[:6]
}
