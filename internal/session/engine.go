package session

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Control protocol on the "game" DataChannel.
const (
	CtrlHello byte = 1
	CtrlPing  byte = 2
	CtrlPong  byte = 3
	CtrlLeave byte = 4
	CtrlGame  byte = 120

	ProtocolVersion byte = 2
)

// Timers and limits of the reference client.
const (
	TickEvery        = 250 * time.Millisecond
	PingEvery        = 500 * time.Millisecond
	Silence          = 2 * time.Second
	RouteWindow      = 60 * time.Second
	JoinTimeout      = 90 * time.Second
	RetryRestart     = 6 * time.Second
	RetryNewLink     = 10 * time.Second
	NewLinkAttempts  = 2
	MaxPeers         = 15
	MaxBuffered      = 64 << 10
	OfferCount       = 6
	OfferKeep        = OfferCount * 4
	RetireKeep       = 32
	OwnerKeep        = 1024
	AnnounceEvery    = 15 * time.Second
	OfferAnnounce    = 2 * time.Second
	ICEConfigEvery   = 30 * time.Minute
	rttSamples       = 600
	statsEvery       = 60 * time.Second
	initialOfferWait = 5 * time.Second
	renewOfferWait   = 3 * time.Second
	routeWait        = 300 * time.Millisecond
)

// Config describes one session.
type Config struct {
	Mode Mode
	Code string

	// RelayURL is the reference rendezvous Worker. Empty means the default.
	RelayURL        string
	DisableRelay    bool
	Trackers        []string
	DisableTrackers bool
	STUN            []string

	PeerID        string
	KDFIterations int
	GamePort      int

	// RelayOnly forces TURN candidates. Like the reference client's test
	// switch, it is useful to prove the relay path works.
	RelayOnly bool

	Logf     func(format string, args ...any)
	OnState  func(State)
	OnGameIn func(bool)

	// DebugPackets logs every game datagram. It is meant for tests and for
	// diagnosing a bridge problem; it would be very noisy while playing.
	DebugPackets bool
}

// Engine is one room. It owns the trackers, the WebRTC links and the UDP
// bridge, and exposes a pollable State.
type Engine struct {
	cfg  Config
	room *Room

	bridge *Bridge
	signal *Signal

	mu        sync.Mutex
	state     State
	slots     map[string]*slot
	slotOrder []string
	offers    map[string]offerRef
	offerIDs  []string
	owners    map[string]string
	ownerIDs  []string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	startedAt    time.Time
	clockStart   time.Time
	stopped      bool
	leaving      bool
	offering     bool
	lastAnnounce time.Time
	gameIn       bool
	cutUntil     time.Time
	dirty        bool

	iceMu     sync.Mutex
	ice       []webrtcICEServer
	iceAt     time.Time
	iceLoaded bool

	notifyCh chan struct{}
	logs     *ringLog

	chainMu  sync.Mutex
	chains   map[string]*peerChain
	answerMu sync.Mutex
}

type peerChain struct {
	mu sync.Mutex
}

type offerRef struct {
	link *link
	gen  int
}

type slot struct {
	id string

	link  *link
	spare *link

	joined  bool
	modern  bool
	leaving bool

	lastHeard time.Time
	lastPing  time.Time
	lostAt    time.Time

	attempts    int
	nextAttempt time.Time

	route     string
	firstSeen time.Time

	retired      map[string]bool
	retiredOrder []string

	cnt  counters
	game *bool
}

type counters struct {
	up, down, dropped, pings, pongs uint64
	totalUp, totalDown              uint64
	rtt                             []uint32
}

// New builds an engine. Nothing listens until Start is called.
func New(cfg Config) *Engine {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	e := &Engine{
		cfg:      cfg,
		slots:    map[string]*slot{},
		offers:   map[string]offerRef{},
		owners:   map[string]string{},
		notifyCh: make(chan struct{}, 1),
		logs:     newRingLog(400),
		chains:   map[string]*peerChain{},
	}
	return e
}

// Start derives the room, opens the game port and begins announcing.
func (e *Engine) Start(parent context.Context) (State, error) {
	if parent == nil {
		parent = context.Background()
	}
	code, err := Normalize(e.cfg.Code)
	if err != nil {
		return State{}, err
	}
	start := time.Now()
	room, err := DeriveWith(code, e.cfg.KDFIterations)
	if err != nil {
		return State{}, err
	}
	e.room = room
	e.clockStart = time.Now()
	e.startedAt = e.clockStart
	e.logf("room ready in %d ms (%s)", time.Since(start).Milliseconds(), room.Describe())

	e.ctx, e.cancel = context.WithCancel(parent)

	e.bridge = NewBridge(BridgeConfig{
		Mode:       e.cfg.Mode,
		GamePort:   e.cfg.GamePort,
		OnPacket:   e.onGamePacket,
		SendToPeer: e.sendToPeer,
		OnGameIn:   func() { e.SetGameIn(true) },
		Logf:       e.logf,
	})
	port, err := e.bridge.Start()
	if err != nil {
		e.cancel()
		return State{}, err
	}

	e.mu.Lock()
	e.state = State{
		Mode:    e.cfg.Mode,
		Code:    code,
		Phase:   PhaseStarting,
		Friends: []Friend{},
		Port:    port,
	}
	if e.cfg.Mode == ModeHost {
		e.state.Phase = PhaseWaiting
	} else {
		e.state.Phase = PhaseConnecting
	}
	e.touchLocked()
	e.mu.Unlock()

	trackers := e.cfg.Trackers
	if len(trackers) == 0 && !e.cfg.DisableTrackers {
		trackers = PublicTrackers
	}
	relay := e.cfg.RelayURL
	if relay == "" && !e.cfg.DisableRelay {
		relay = DefaultRelayURL
	}
	e.signal = NewSignal(room, e.cfg.PeerID, relay, trackers, SignalEvents{
		OnOffer:  func(peerID, sealed, offerID, via string) { e.queueOffer(peerID, sealed, offerID, via) },
		OnAnswer: func(peerID, sealed, offerID string) { e.queueAnswer(peerID, sealed, offerID) },
		OnStatus: e.onTrackers,
		Logf:     e.logf,
	})
	e.signal.Start(e.ctx)

	e.wg.Add(3)
	go e.tickLoop()
	go e.statsLoop()
	go e.notifyLoop()

	if e.cfg.Mode == ModeJoin {
		e.mu.Lock()
		s := e.newSlotLocked("host")
		e.touchLocked()
		e.mu.Unlock()
		go func() {
			if err := e.offerNew(s); err != nil {
				e.logf("couldn't make the first offer: %v", err)
			}
		}()
	}

	e.mu.Lock()
	st := e.snapshotLocked()
	e.mu.Unlock()
	return st, nil
}

// State returns a copy of the public session state.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// Port is the UDP port the local game uses.
func (e *Engine) Port() int {
	if e.bridge == nil {
		return 0
	}
	return e.bridge.Port()
}

// Logs returns the recent engine log.
func (e *Engine) Logs() []string { return e.logs.Lines() }

// SetGameIn tells the session whether the local game has opened.
func (e *Engine) SetGameIn(in bool) {
	e.mu.Lock()
	if e.gameIn == in {
		e.mu.Unlock()
		return
	}
	e.gameIn = in
	e.state.GameIn = in
	e.touchLocked()
	cb := e.cfg.OnGameIn
	e.mu.Unlock()
	if cb != nil {
		cb(in)
	}
}

// GameExited marks the local game as out. The room and its links stay up, so
// the same code keeps working when the game is opened again.
func (e *Engine) GameExited() {
	e.mu.Lock()
	if e.stopped || e.leaving {
		e.mu.Unlock()
		return
	}
	changed := e.gameIn
	e.gameIn = false
	e.state.GameIn = false
	if changed {
		e.touchLocked()
	}
	cb := e.cfg.OnGameIn
	e.mu.Unlock()
	if e.bridge != nil {
		e.bridge.ResetGame()
	}
	if changed && cb != nil {
		cb(false)
	}
	e.logf("the game closed; the connection stays, the game can open again")
}

// CutForTest silently drops game traffic for d, as the reference client's
// test hook does. It is exported so integration tests can exercise holds.
func (e *Engine) CutForTest(d time.Duration) {
	e.mu.Lock()
	e.cutUntil = time.Now().Add(d)
	e.mu.Unlock()
}

// Stop sends leave, closes every link and releases the sockets.
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.stopped || e.leaving {
		e.mu.Unlock()
		return
	}
	e.leaving = true
	var live []*link
	allEmpty := true
	for _, s := range e.slots {
		for _, l := range []*link{s.link, s.spare} {
			if l != nil && l.dc != nil && l.dc.ReadyState() == dataChannelOpen {
				live = append(live, l)
				if l.dc.BufferedAmount() > 0 {
					allEmpty = false
				}
			}
		}
	}
	e.mu.Unlock()

	for _, l := range live {
		e.control(l, []byte{CtrlLeave})
	}
	if len(live) > 0 {
		if allEmpty {
			time.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(400 * time.Millisecond)
		}
	}

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	e.state.Phase = PhaseStopped
	e.state.Friends = []Friend{}
	e.touchLocked()
	cancel := e.cancel
	var links []*link
	for _, s := range e.slots {
		if s.link != nil {
			links = append(links, s.link)
		}
		if s.spare != nil {
			links = append(links, s.spare)
		}
	}
	e.slots = map[string]*slot{}
	e.slotOrder = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if e.signal != nil {
		e.signal.Close()
	}
	for _, l := range links {
		_ = l.pc.Close()
	}
	if e.bridge != nil {
		e.bridge.Close()
	}
	e.wg.Wait()
	e.logf("session end")
}

// --- lifecycle helpers -----------------------------------------------------

func (e *Engine) tickLoop() {
	defer e.wg.Done()
	t := time.NewTicker(TickEvery)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.tick()
		}
	}
}

func (e *Engine) statsLoop() {
	defer e.wg.Done()
	t := time.NewTicker(statsEvery)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.logMinute()
		}
	}
}

func (e *Engine) notifyLoop() {
	defer e.wg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.notifyCh:
			e.mu.Lock()
			st := e.snapshotLocked()
			e.dirty = false
			cb := e.cfg.OnState
			e.mu.Unlock()
			if cb != nil {
				cb(st)
			}
		}
	}
}

func (e *Engine) touchLocked() {
	e.dirty = true
	select {
	case e.notifyCh <- struct{}{}:
	default:
	}
}

func (e *Engine) snapshotLocked() State {
	st := e.state
	st.Friends = e.friendListLocked()
	if st.Message != nil {
		msg := *st.Message
		st.Message = &msg
	}
	return st
}

func (e *Engine) friendListLocked() []Friend {
	list := make([]Friend, 0, len(e.slots))
	for _, s := range e.slots {
		if !s.joined {
			continue
		}
		f := Friend{
			ID:           s.id,
			Reconnecting: !s.lostAt.IsZero(),
		}
		if s.route != "" {
			f.Route = strPtr(s.route)
		}
		if s.game != nil {
			f.Game = boolPtr(*s.game)
		}
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Reconnecting != list[j].Reconnecting {
			return !list[i].Reconnecting
		}
		return list[i].ID < list[j].ID
	})
	return list
}

func (e *Engine) newSlotLocked(id string) *slot {
	now := time.Now()
	s := &slot{
		id:        id,
		firstSeen: now,
		lastHeard: now,
		retired:   map[string]bool{},
	}
	e.slots[id] = s
	e.slotOrder = append(e.slotOrder, id)
	return s
}

func (e *Engine) removeSlotLocked(id string) *slot {
	s := e.slots[id]
	if s == nil {
		return nil
	}
	delete(e.slots, id)
	for i, v := range e.slotOrder {
		if v == id {
			e.slotOrder = append(e.slotOrder[:i], e.slotOrder[i+1:]...)
			break
		}
	}
	return s
}

func (e *Engine) failLocked(msg string) {
	if e.state.Phase == PhaseFailed {
		return
	}
	e.state.Phase = PhaseFailed
	e.state.Message = strPtr(msg)
	e.touchLocked()
	e.logf("session failed: %s", msg)
}

func (e *Engine) fail(msg string) {
	e.mu.Lock()
	e.failLocked(msg)
	e.mu.Unlock()
}

func (e *Engine) onTrackers(open int) {
	e.mu.Lock()
	e.state.Trackers = open
	e.touchLocked()
	e.mu.Unlock()
}

func (e *Engine) logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	e.logs.Add(line)
	e.cfg.Logf("%s", line)
}

// --- tick ------------------------------------------------------------------

func (e *Engine) tick() {
	now := time.Now()

	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	announce := false
	if e.offering && now.Sub(e.lastAnnounce) >= OfferAnnounce {
		e.lastAnnounce = now
		announce = true
	}
	if e.state.Mode == ModeJoin && e.state.Phase == PhaseConnecting && now.Sub(e.startedAt) > JoinTimeout {
		e.failLocked("Couldn’t find your friend. Check the code, that they’re hosting right now, and that you both have Coop Mod Loader 1.3 or newer.")
		e.mu.Unlock()
		if announce {
			e.signal.Announce()
		}
		return
	}

	type action struct {
		kind   string
		id     string
		reason string
	}
	var actions []action
	var pings []*link
	gameIn := e.gameIn
	mode := e.state.Mode

	for _, id := range append([]string(nil), e.slotOrder...) {
		s := e.slots[id]
		if s == nil || s.leaving {
			continue
		}
		l := s.link
		if l != nil && l.dc != nil && l.dc.ReadyState() == dataChannelOpen &&
			s.modern && now.Sub(s.lastPing) >= PingEvery {
			s.lastPing = now
			s.cnt.pings++
			pings = append(pings, l)
		}
		if !s.joined {
			if l != nil && l.pc.ConnectionState() == peerFailed {
				actions = append(actions, action{kind: "notReached", id: id, reason: "no route found"})
			} else if e.state.Mode == ModeHost && now.Sub(s.firstSeen) > RouteWindow {
				actions = append(actions, action{kind: "drop", id: id, reason: "never connected"})
			}
			continue
		}
		up := e.upLocked(s)
		if !up && s.lostAt.IsZero() {
			reason := "closed"
			if l != nil && l.dc != nil && l.dc.ReadyState() == dataChannelOpen {
				reason = "quiet"
			}
			e.lostLocked(s, reason)
		} else if up && !s.lostAt.IsZero() {
			e.backLocked(s)
		}
		if !s.lostAt.IsZero() {
			if now.Sub(s.lostAt) > RouteWindow {
				actions = append(actions, action{kind: "drop", id: id, reason: "no route back within 60 s"})
				continue
			}
			if e.state.Mode == ModeJoin && !now.Before(s.nextAttempt) {
				actions = append(actions, action{kind: "attempt", id: id})
			}
		}
	}
	e.mu.Unlock()

	if announce {
		e.signal.Announce()
	}
	for _, l := range pings {
		e.control(l, pingPacket(e.now32(), gameIn))
	}
	for _, a := range actions {
		switch a.kind {
		case "attempt":
			e.attempt(a.id)
		case "drop":
			e.drop(a.id, a.reason)
		case "notReached":
			if mode == ModeJoin {
				e.fail("Couldn’t connect to your friend, not even through the relay.")
			}
			e.drop(a.id, a.reason)
		}
	}
}

func (e *Engine) upLocked(s *slot) bool {
	l := s.link
	if l == nil || l.dc == nil || l.dc.ReadyState() != dataChannelOpen {
		return false
	}
	state := l.pc.ConnectionState()
	if state == peerFailed || state == peerClosed {
		return false
	}
	if s.modern {
		return time.Since(s.lastHeard) <= Silence
	}
	return state != peerDisconnected
}

func (e *Engine) lostLocked(s *slot, why string) {
	if !s.lostAt.IsZero() {
		return
	}
	s.lostAt = time.Now()
	s.attempts = 0
	s.nextAttempt = s.lostAt
	e.touchLocked()
	e.logf("%s: link lost (%s), holding their place", short(s.id), why)
	if e.bridge != nil {
		e.bridge.SetLink(s.id, false)
	}
}

func (e *Engine) backLocked(s *slot) {
	if s.lostAt.IsZero() {
		return
	}
	elapsed := time.Since(s.lostAt)
	s.lostAt = time.Time{}
	e.touchLocked()
	if e.bridge != nil {
		e.bridge.SetLink(s.id, true)
	}
	l := s.link
	e.logf("%s: back after %.1f s", short(s.id), elapsed.Seconds())
	if l != nil {
		go func() {
			route, detail := routeStats(l.pc)
			e.mu.Lock()
			if e.slots[s.id] == s {
				if route != "" {
					s.route = route
				}
				e.touchLocked()
			}
			e.mu.Unlock()
			if detail != "" {
				e.logf("%s: back route %s", short(s.id), detail)
			}
		}()
	}
}

func (e *Engine) attempt(id string) {
	e.mu.Lock()
	s := e.slots[id]
	if s == nil || s.lostAt.IsZero() || e.stopped || e.leaving {
		e.mu.Unlock()
		return
	}
	if sp := s.spare; sp != nil && !sp.answeredAt.IsZero() {
		st := sp.pc.ConnectionState()
		if st != peerFailed && st != peerClosed {
			at := sp.answeredAt.Add(RetryNewLink)
			if time.Now().Before(at) {
				s.nextAttempt = at
				e.mu.Unlock()
				return
			}
		}
	}
	i := s.attempts
	s.attempts++
	needNew := s.link == nil || s.link.dc == nil ||
		s.link.dc.ReadyState() != dataChannelOpen ||
		s.link.pc.ConnectionState() == peerClosed
	useNew := s.modern && (needNew || i >= NewLinkAttempts)
	if needNew && !s.modern {
		e.mu.Unlock()
		return
	}
	if useNew {
		s.nextAttempt = time.Now().Add(RetryNewLink)
	} else {
		s.nextAttempt = time.Now().Add(RetryRestart)
	}
	e.mu.Unlock()

	if useNew {
		_ = e.offerNew(s)
		return
	}
	_ = e.restart(s)
}

func (e *Engine) drop(id, why string) {
	e.mu.Lock()
	s := e.slots[id]
	if s == nil {
		e.mu.Unlock()
		return
	}
	e.removeSlotLocked(id)
	mode := e.state.Mode
	leaving := s.leaving
	var links []*link
	if s.link != nil {
		links = append(links, s.link)
	}
	if s.spare != nil {
		links = append(links, s.spare)
	}
	e.logf("%s: dropped (%s)", short(id), why)
	if mode == ModeHost {
		if len(e.slots) > 0 {
			e.state.Phase = PhaseConnected
		} else {
			e.state.Phase = PhaseWaiting
		}
	} else if e.state.Phase == PhaseConnected {
		if leaving {
			e.failLocked("Your friend stopped hosting.")
		} else {
			e.failLocked("The connection to your friend was lost.")
		}
	}
	e.touchLocked()
	e.mu.Unlock()

	for _, l := range links {
		_ = l.pc.Close()
	}
	if e.bridge != nil {
		e.bridge.PeerClose(id)
	}
}

func (e *Engine) logMinute() {
	type statsLine struct {
		id      string
		state   string
		detail  string
		up      uint64
		down    uint64
		dropped uint64
		pings   uint64
		pongs   uint64
		rttMin  uint32
		rttMax  uint32
		modern  bool
	}
	e.mu.Lock()
	var lines []statsLine
	for _, s := range e.slots {
		line := statsLine{
			id:      s.id,
			up:      s.cnt.up,
			down:    s.cnt.down,
			dropped: s.cnt.dropped,
			pings:   s.cnt.pings,
			pongs:   s.cnt.pongs,
			modern:  s.modern,
		}
		if len(s.cnt.rtt) > 0 {
			line.rttMin, line.rttMax = s.cnt.rtt[0], s.cnt.rtt[0]
			for _, v := range s.cnt.rtt {
				if v < line.rttMin {
					line.rttMin = v
				}
				if v > line.rttMax {
					line.rttMax = v
				}
			}
		}
		if s.link != nil {
			line.state = s.link.pc.ConnectionState().String()
		} else {
			line.state = "no link"
		}
		s.cnt = counters{}
		lines = append(lines, line)
	}
	e.mu.Unlock()

	if len(lines) == 0 {
		e.logf("last minute: no friends")
		return
	}
	var out string
	for i, ln := range lines {
		route := "-"
		if link := e.linkFor(ln.id); link != nil {
			if _, detail := routeStats(link.pc); detail != "" {
				route = detail
			}
		}
		beat := ""
		if ln.modern {
			beat = fmt.Sprintf(", heartbeat %d/%d", ln.pongs, ln.pings)
			if ln.rttMax > 0 {
				beat += fmt.Sprintf(" (%d-%d ms)", ln.rttMin, ln.rttMax)
			}
		}
		if i > 0 {
			out += "; "
		}
		out += fmt.Sprintf("%s %s %s: %d sent, %d received, %d dropped%s",
			short(ln.id), ln.state, route, ln.up, ln.down, ln.dropped, beat)
	}
	e.logf("last minute: %s", out)
}

func (e *Engine) linkFor(id string) *link {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.slots[id]; s != nil {
		return s.link
	}
	return nil
}

func (e *Engine) now32() uint32 {
	return uint32(time.Since(e.clockStart).Milliseconds())
}

func pingPacket(ts uint32, gameIn bool) []byte {
	b := make([]byte, 6)
	b[0] = CtrlPing
	binary.LittleEndian.PutUint32(b[1:5], ts)
	if gameIn {
		b[5] = 1
	}
	return b
}

// --- game traffic ----------------------------------------------------------

func (e *Engine) onGamePacket(peerID string, data []byte) {
	e.sendToPeer(peerID, data)
}

func (e *Engine) sendToPeer(peerID string, data []byte) {
	e.mu.Lock()
	if e.stopped || e.cut() {
		e.mu.Unlock()
		return
	}
	s := e.slots[peerID]
	if s == nil || s.link == nil {
		e.mu.Unlock()
		if e.cfg.DebugPackets {
			e.logf("game->%s: no link for %d bytes", short(peerID), len(data))
		}
		return
	}
	l := s.link
	// Do not wait for a full retransmission queue: game traffic is realtime.
	ok := l.dc != nil && l.dc.ReadyState() == dataChannelOpen && l.dc.BufferedAmount() < MaxBuffered
	if ok {
		s.cnt.up++
		s.cnt.totalUp++
	} else {
		s.cnt.dropped++
	}
	e.mu.Unlock()
	if e.cfg.DebugPackets {
		e.logf("game->%s: %d bytes (send=%v)", short(peerID), len(data), ok)
	}
	if ok {
		if err := l.dc.Send(data); err != nil {
			e.logf("%s: couldn't send to the friend: %v", short(peerID), err)
			e.mu.Lock()
			if cur := e.slots[peerID]; cur == s {
				s.cnt.dropped++
			}
			e.mu.Unlock()
		}
	}
}

func (e *Engine) cut() bool {
	return time.Now().Before(e.cutUntil)
}

func (e *Engine) control(l *link, data []byte) {
	if l == nil || l.dc == nil || l.dc.ReadyState() != dataChannelOpen {
		return
	}
	e.mu.Lock()
	cut := e.cut()
	e.mu.Unlock()
	if cut {
		return
	}
	_ = l.dc.Send(data)
}

// onMessage is the DataChannel receive path. It mirrors the reference
// client's control handling byte for byte.
func (e *Engine) onMessage(id string, l *link, data []byte) {
	if len(data) == 0 {
		return
	}
	copied := append([]byte(nil), data...)

	e.mu.Lock()
	if e.stopped || e.cut() {
		e.mu.Unlock()
		return
	}
	s := e.slots[id]
	if s == nil || (s.link != l && s.spare != l) {
		e.mu.Unlock()
		return
	}
	if len(copied) > MaxGamePacket {
		s.cnt.dropped++
		e.mu.Unlock()
		return
	}
	s.lastHeard = time.Now()

	emit := false
	var pong []byte
	forward := false

	switch copied[0] {
	case CtrlGame:
		s.cnt.down++
		s.cnt.totalDown++
		forward = true
		if e.cfg.DebugPackets {
			e.logf("%s->game: %d bytes", short(s.id), len(copied))
		}
	case CtrlHello:
		if !s.modern {
			version := byte(0)
			if len(copied) >= 2 {
				version = copied[1]
			}
			e.logf("%s: speaks heartbeat (v%d)", short(s.id), version)
		}
		s.modern = true
	case CtrlPing:
		if len(copied) >= 5 {
			pong = []byte{CtrlPong, copied[1], copied[2], copied[3], copied[4]}
		}
		if len(copied) >= 6 {
			game := copied[5] == 1
			if s.game == nil || *s.game != game {
				s.game = boolPtr(game)
				e.logf("%s: their game is %s", short(s.id), map[bool]string{true: "in", false: "not in yet"}[game])
				emit = true
			}
		}
	case CtrlPong:
		if len(copied) >= 5 {
			ts := binary.LittleEndian.Uint32(copied[1:5])
			s.cnt.pongs++
			if len(s.cnt.rtt) < rttSamples {
				s.cnt.rtt = append(s.cnt.rtt, e.now32()-ts)
			}
		}
	case CtrlLeave:
		s.leaving = true
		e.mu.Unlock()
		e.drop(id, "they left")
		return
	}

	if !s.lostAt.IsZero() {
		up := e.upLocked(s)
		if up {
			e.backLocked(s)
		}
	}
	if emit {
		e.touchLocked()
	}
	e.mu.Unlock()

	if pong != nil {
		e.control(l, pong)
	}
	if forward && e.bridge != nil {
		if err := e.bridge.SendToGame(id, copied); err != nil {
			e.logf("%s: couldn't give the game a packet: %v", short(id), err)
			e.mu.Lock()
			if cur := e.slots[id]; cur == s {
				s.cnt.dropped++
			}
			e.mu.Unlock()
		}
	}
}

// --- signalling ------------------------------------------------------------

func (e *Engine) queueOffer(peerID, sealed, offerID, via string) {
	chain := e.chainFor(peerID)
	go func() {
		chain.mu.Lock()
		defer chain.mu.Unlock()
		e.handleOffer(peerID, sealed, offerID, via)
	}()
}

func (e *Engine) chainFor(peerID string) *peerChain {
	e.chainMu.Lock()
	defer e.chainMu.Unlock()
	c := e.chains[peerID]
	if c == nil {
		c = &peerChain{}
		e.chains[peerID] = c
	}
	return c
}

func (e *Engine) queueAnswer(peerID, sealed, offerID string) {
	go func() {
		e.answerMu.Lock()
		defer e.answerMu.Unlock()
		e.handleAnswer(peerID, sealed, offerID)
	}()
}

// ringLog keeps a bounded history for the UI.
type ringLog struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) Add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = append([]string(nil), r.lines[len(r.lines)-r.max:]...)
	}
}

func (r *ringLog) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func randomID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(i * 11)
		}
	}
	out := make([]byte, n)
	for i, v := range buf {
		out[i] = 'A' + v%26
	}
	return string(out)
}
