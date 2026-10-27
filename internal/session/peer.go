package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Short aliases keep the state machine readable.
var (
	dataChannelOpen  = webrtc.DataChannelStateOpen
	peerFailed       = webrtc.PeerConnectionStateFailed
	peerClosed       = webrtc.PeerConnectionStateClosed
	peerDisconnected = webrtc.PeerConnectionStateDisconnected
)

type webrtcICEServer = webrtc.ICEServer

// link is one PeerConnection. A slot can hold an active link and a spare
// link while a replacement route is being built.
type link struct {
	pc *webrtc.PeerConnection
	dc *webrtc.DataChannel

	fingerprint string
	gen         int

	seen      map[string]bool
	seenOrder []string

	answeredGen    int
	answeredAt     time.Time
	answeredKey    string
	answeredSealed string
}

// defaultSTUN is the pair the reference client always starts with.
func defaultSTUN() []string {
	return []string{
		"stun:stun.l.google.com:19302",
		"stun:stun.cloudflare.com:3478",
	}
}

func (e *Engine) iceServers(ctx context.Context) ([]webrtcICEServer, error) {
	e.iceMu.Lock()
	defer e.iceMu.Unlock()

	base := e.cfg.STUN
	if len(base) == 0 {
		base = defaultSTUN()
	}
	servers := make([]webrtcICEServer, 0, len(base)+1)
	for _, stun := range base {
		servers = append(servers, webrtcICEServer{URLs: []string{stun}})
	}

	if e.iceLoaded && time.Since(e.iceAt) < ICEConfigEvery {
		return e.ice, nil
	}

	relay := e.cfg.RelayURL
	if relay == "" && !e.cfg.DisableRelay {
		relay = DefaultRelayURL
	}
	if relay == "" {
		e.ice = servers
		e.iceAt = time.Now()
		e.iceLoaded = true
		return servers, nil
	}

	fetched, err := fetchICEServers(ctx, relay)
	if err != nil {
		e.logf("relay logins unavailable right now; direct routes only (%v)", err)
	} else {
		servers = append(servers, fetched...)
	}
	e.ice = servers
	e.iceAt = time.Now()
	e.iceLoaded = true
	return servers, nil
}

func (e *Engine) currentICEServers() []webrtcICEServer {
	e.iceMu.Lock()
	defer e.iceMu.Unlock()
	if len(e.ice) == 0 {
		servers := make([]webrtcICEServer, 0, 2)
		for _, stun := range defaultSTUN() {
			servers = append(servers, webrtcICEServer{URLs: []string{stun}})
		}
		return servers
	}
	return append([]webrtcICEServer(nil), e.ice...)
}

func (e *Engine) relayed() bool {
	e.iceMu.Lock()
	defer e.iceMu.Unlock()
	for _, s := range e.ice {
		for _, u := range s.URLs {
			lower := strings.ToLower(u)
			if strings.HasPrefix(lower, "turn:") || strings.HasPrefix(lower, "turns:") {
				return true
			}
		}
	}
	return false
}

type iceJSON struct {
	ICEServers []struct {
		URLs       json.RawMessage `json:"urls"`
		Username   string          `json:"username"`
		Credential string          `json:"credential"`
	} `json:"iceServers"`
}

func fetchICEServers(ctx context.Context, relay string) ([]webrtcICEServer, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(relay, "/")+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("relay answered %d", resp.StatusCode)
	}
	var doc iceJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	out := make([]webrtcICEServer, 0, len(doc.ICEServers))
	for _, s := range doc.ICEServers {
		var one string
		var many []string
		switch {
		case json.Unmarshal(s.URLs, &one) == nil:
			many = []string{one}
		case json.Unmarshal(s.URLs, &many) == nil:
		default:
			continue
		}
		var urls []string
		for _, u := range many {
			u = strings.TrimSpace(u)
			if u != "" {
				urls = append(urls, u)
			}
		}
		if len(urls) == 0 {
			continue
		}
		out = append(out, webrtcICEServer{
			URLs:       urls,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out, nil
}

// newPeerLink builds a PeerConnection with the project's ICE settings.
func (e *Engine) newPeerLink(id, fingerprint string) (*link, error) {
	setting := webrtc.SettingEngine{}
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(setting))

	cfg := webrtc.Configuration{
		ICEServers:         e.currentICEServers(),
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
	}
	if e.cfg.RelayOnly {
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	l := &link{
		pc:          pc,
		fingerprint: fingerprint,
		seen:        map[string]bool{},
	}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		e.logf("%s: connection %s", short(id), state)
		if e.ownsLink(id, l) {
			e.check(id)
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		e.logf("%s: ice %s", short(id), state)
	})
	return l, nil
}

// wire attaches the DataChannel callbacks.
func (e *Engine) wire(id string, l *link, dc *webrtc.DataChannel) {
	e.mu.Lock()
	l.dc = dc
	e.mu.Unlock()

	dc.OnOpen(func() {
		e.opened(id, l)
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		e.onMessage(id, l, msg.Data)
	})
	dc.OnClose(func() {
		e.closed(id, l)
	})
}

func (e *Engine) ownsLink(id string, l *link) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.slots[id]
	return s != nil && (s.link == l || s.spare == l)
}

func (e *Engine) opened(id string, l *link) {
	e.mu.Lock()
	s := e.slots[id]
	if s == nil || (s.link != l && s.spare != l) {
		e.mu.Unlock()
		return
	}
	if s.spare == l {
		old := s.link
		s.link = l
		s.spare = nil
		e.retireLocked(s, old)
		if old != nil {
			go func() { _ = old.pc.Close() }()
		}
		e.logf("%s: moved to the new connection", short(id))
	}
	if s.link != l {
		e.mu.Unlock()
		return
	}
	first := !s.joined
	host := e.state.Mode == ModeHost
	s.joined = true
	// An open channel is proof the link is alive; without this a modern peer
	// would be marked lost until its next heartbeat, 500 ms later.
	s.lastHeard = time.Now()
	if first {
		e.state.Phase = PhaseConnected
		e.state.Message = nil
	}
	e.touchLocked()
	e.mu.Unlock()

	e.control(l, []byte{CtrlHello, ProtocolVersion})
	if host && first && e.bridge != nil {
		if _, err := e.bridge.PeerOpen(id); err != nil {
			e.logf("%s: couldn't open the game port: %v", short(id), err)
		}
	}
	e.logf("%s: game channel open", short(id))

	// Route statistics can lag a few hundred milliseconds behind OnOpen.
	// Do not hold the channel callback while waiting for them.
	go func() {
		route, detail := waitRoute(l.pc)
		e.mu.Lock()
		if e.slots[id] == s && s.link == l {
			if route != "" {
				s.route = route
			}
			e.touchLocked()
		}
		e.mu.Unlock()
		if detail == "" {
			detail = "route unknown"
		}
		e.logf("%s: route %s", short(id), detail)
	}()

	e.check(id)
}

func (e *Engine) closed(id string, l *link) {
	e.mu.Lock()
	s := e.slots[id]
	if s == nil || s.leaving {
		e.mu.Unlock()
		return
	}
	if s.spare == l {
		s.spare = nil
		e.mu.Unlock()
		return
	}
	if s.link != l {
		e.mu.Unlock()
		return
	}
	modern := s.modern
	e.mu.Unlock()

	e.logf("%s: game channel closed", short(id))
	if modern {
		e.check(id)
		return
	}
	e.drop(id, "game channel closed")
}

func (e *Engine) retireLocked(s *slot, l *link) {
	if l == nil || l.fingerprint == "" {
		return
	}
	if s.retired == nil {
		s.retired = map[string]bool{}
	}
	if !s.retired[l.fingerprint] {
		s.retired[l.fingerprint] = true
		s.retiredOrder = append(s.retiredOrder, l.fingerprint)
	}
	for len(s.retiredOrder) > RetireKeep {
		oldest := s.retiredOrder[0]
		s.retiredOrder = s.retiredOrder[1:]
		delete(s.retired, oldest)
	}
}

func (e *Engine) check(id string) {
	e.mu.Lock()
	s := e.slots[id]
	if s == nil || !s.joined || s.leaving || e.stopped || e.leaving {
		e.mu.Unlock()
		return
	}
	up := e.upLocked(s)
	if !up && s.lostAt.IsZero() {
		why := "closed"
		if s.link != nil && s.link.dc != nil && s.link.dc.ReadyState() == dataChannelOpen {
			why = "quiet"
		}
		e.lostLocked(s, why)
		e.mu.Unlock()
		return
	}
	if up && !s.lostAt.IsZero() {
		e.backLocked(s)
	}
	e.mu.Unlock()
}

// --- offers and answers ----------------------------------------------------

func (e *Engine) offerNew(s *slot) error {
	if s == nil {
		return errors.New("no slot")
	}
	if _, err := e.iceServers(e.ctx); err != nil {
		e.logf("%s: ICE lookup failed: %v", short(s.id), err)
	}
	l, err := e.newPeerLink(s.id, "")
	if err != nil {
		return err
	}

	ordered := false
	retransmits := uint16(0)
	dc, err := l.pc.CreateDataChannel("game", &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &retransmits,
	})
	if err != nil {
		_ = l.pc.Close()
		return err
	}
	e.wire(s.id, l, dc)

	e.mu.Lock()
	cur := e.slots[s.id]
	if cur == nil || e.stopped || e.leaving {
		e.mu.Unlock()
		_ = l.pc.Close()
		return errors.New("that friend is gone")
	}
	var old *link
	if cur.link != nil && cur.joined {
		old = cur.spare
		cur.spare = l
	} else {
		old = cur.link
		cur.link = l
	}
	e.retireLocked(cur, old)
	e.mu.Unlock()
	if old != nil {
		_ = old.pc.Close()
	}

	offer, err := l.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	wait := startGather(l.pc, gatherTimeout(cur != nil && cur.joined), e.relayed())
	if err := l.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	wait.Wait()

	if !e.ownsLink(s.id, l) {
		return errors.New("the connection was replaced")
	}
	if err := e.publishOffer(l); err != nil {
		return err
	}
	if e.slotJoined(s.id) {
		e.logf("%s: building a new connection", short(s.id))
	}
	return nil
}

func (e *Engine) restart(s *slot) error {
	e.mu.Lock()
	l := s.link
	joined := s.joined
	e.mu.Unlock()
	if l == nil {
		return nil
	}
	servers, _ := e.iceServers(e.ctx)
	cfg := l.pc.GetConfiguration()
	cfg.ICEServers = servers
	_ = l.pc.SetConfiguration(cfg)

	if l.pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		_ = l.pc.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeRollback})
	}
	offer, err := l.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		return err
	}
	wait := startGather(l.pc, gatherTimeout(joined), e.relayed())
	if err := l.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	wait.Wait()

	e.mu.Lock()
	cur := e.slots[s.id]
	still := cur == s && s.link == l && !s.lostAt.IsZero() && !e.stopped && !e.leaving
	e.mu.Unlock()
	if !still {
		return nil
	}
	if err := e.publishOffer(l); err != nil {
		return err
	}
	e.logf("%s: asked for a new route", short(s.id))
	return nil
}

func gatherTimeout(joined bool) time.Duration {
	if joined {
		return renewOfferWait
	}
	return initialOfferWait
}

func (e *Engine) slotJoined(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.slots[id]
	return s != nil && s.joined
}

func (e *Engine) publishOffer(l *link) error {
	if l == nil || l.pc.LocalDescription() == nil {
		return errors.New("no local description")
	}
	sealed, err := e.room.Seal(l.pc.LocalDescription().SDP)
	if err != nil {
		return err
	}

	e.mu.Lock()
	if e.stopped || e.leaving {
		e.mu.Unlock()
		return errors.New("the session ended")
	}
	l.gen++
	gen := l.gen
	offers := make([]Offer, 0, OfferCount)
	for i := 0; i < OfferCount; i++ {
		id := randomID(20)
		offers = append(offers, Offer{OfferID: id, SealedSDP: sealed})
		e.offers[id] = offerRef{link: l, gen: gen}
		e.offerIDs = append(e.offerIDs, id)
	}
	for len(e.offerIDs) > OfferKeep {
		oldest := e.offerIDs[0]
		e.offerIDs = e.offerIDs[1:]
		delete(e.offers, oldest)
	}
	e.offering = true
	e.lastAnnounce = time.Now()
	e.mu.Unlock()

	e.signal.SetOffers(offers, true)
	return nil
}

func (e *Engine) handleAnswer(peerID, sealed, offerID string) {
	e.mu.Lock()
	if e.state.Mode != ModeJoin {
		e.mu.Unlock()
		return
	}
	ref, ok := e.offers[offerID]
	if !ok || ref.link == nil {
		e.mu.Unlock()
		return
	}
	l := ref.link
	if ref.gen != l.gen || l.pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		e.mu.Unlock()
		return
	}
	if l.answeredGen == ref.gen {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	sdp, ok := e.room.Open(sealed)
	if !ok {
		return
	}

	e.mu.Lock()
	if ref.gen != l.gen || l.pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer ||
		l.answeredGen == ref.gen {
		e.mu.Unlock()
		return
	}
	l.answeredGen = ref.gen
	l.answeredAt = time.Now()
	e.offering = false
	e.mu.Unlock()

	e.signal.SetOffers(nil, false)
	if err := l.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		e.logf("couldn't use the host's answer (%v)", err)
		return
	}
	e.logf("host answered")
}

func (e *Engine) handleOffer(peerID, sealed, offerID, via string) {
	e.logf("offer from %s via %s", short(peerID), via)
	e.mu.Lock()
	host := e.state.Mode == ModeHost && !e.stopped && !e.leaving
	e.mu.Unlock()
	if !host {
		return
	}
	sdp, ok := e.room.Open(sealed)
	if !ok {
		e.logf("offer from %s could not be opened", short(peerID))
		return
	}
	fingerprint := sdpAttribute(sdp, "fingerprint")
	ufrag := sdpAttribute(sdp, "ice-ufrag")
	if fingerprint == "" {
		return
	}
	key := fingerprint + "/" + ufrag

	e.mu.Lock()
	if owner, ok := e.owners[fingerprint]; ok && owner != peerID {
		e.mu.Unlock()
		return
	}
	if _, ok := e.owners[fingerprint]; !ok {
		e.owners[fingerprint] = peerID
		e.ownerIDs = append(e.ownerIDs, fingerprint)
		for len(e.ownerIDs) > OwnerKeep {
			oldest := e.ownerIDs[0]
			e.ownerIDs = e.ownerIDs[1:]
			delete(e.owners, oldest)
		}
	}
	s := e.slots[peerID]
	if s != nil && s.retired[fingerprint] {
		e.mu.Unlock()
		return
	}
	var candidates []*link
	if s != nil {
		candidates = []*link{s.link, s.spare}
	}
	for _, l := range candidates {
		if l == nil {
			continue
		}
		if l.answeredKey == key && l.answeredSealed != "" {
			sealedAnswer := l.answeredSealed
			e.mu.Unlock()
			e.signal.Answer(peerID, offerID, sealedAnswer)
			return
		}
		if l.seen[key] {
			e.mu.Unlock()
			return
		}
	}
	var existing *link
	for _, l := range candidates {
		if l != nil && l.fingerprint == fingerprint {
			existing = l
			break
		}
	}
	if existing != nil {
		existing.seen[key] = true
		e.mu.Unlock()
		e.logf("%s: new route offer (%s via %s), answering", short(peerID), short(ufrag), via)
		e.renegotiate(peerID, existing, sdp, key, offerID)
		return
	}
	if s == nil && len(e.slots) >= MaxPeers {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	_, _ = e.iceServers(e.ctx)
	l, err := e.newPeerLink(peerID, fingerprint)
	if err != nil {
		e.logf("%s: couldn't create a connection: %v", short(peerID), err)
		return
	}
	l.seen[key] = true
	l.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "game" {
			return
		}
		e.wire(peerID, l, dc)
	})

	e.mu.Lock()
	if e.stopped || e.leaving {
		e.mu.Unlock()
		_ = l.pc.Close()
		return
	}
	cur := e.slots[peerID]
	existed := cur != nil
	if cur == nil {
		cur = e.newSlotLocked(peerID)
	}
	if cur.retired[fingerprint] {
		e.mu.Unlock()
		_ = l.pc.Close()
		return
	}
	var old *link
	if cur.link != nil && cur.joined {
		old = cur.spare
		cur.spare = l
	} else {
		old = cur.link
		cur.link = l
	}
	e.retireLocked(cur, old)
	if existed {
		e.logf("%s: offered a new connection", short(peerID))
	} else {
		e.logf("friend %s is connecting", short(peerID))
	}
	e.mu.Unlock()
	if old != nil {
		_ = old.pc.Close()
	}
	// Open the friend's private game port as soon as the offer is accepted.
	// Waiting for the channel to open races the first packet from the game.
	if e.bridge != nil {
		if _, err := e.bridge.PeerOpen(peerID); err != nil {
			e.logf("%s: couldn't open the game port: %v", short(peerID), err)
		}
	}

	if err := l.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		e.logf("%s: couldn't read the offer (%v)", short(peerID), err)
		return
	}
	answer, err := l.pc.CreateAnswer(nil)
	if err != nil {
		e.logf("%s: couldn't make an answer (%v)", short(peerID), err)
		return
	}
	wait := startGather(l.pc, gatherTimeout(e.slotJoined(peerID)), e.relayed())
	if err := l.pc.SetLocalDescription(answer); err != nil {
		e.logf("%s: couldn't set the answer (%v)", short(peerID), err)
		return
	}
	wait.Wait()

	if !e.ownsLink(peerID, l) || l.pc.LocalDescription() == nil {
		return
	}
	sealedAnswer, err := e.room.Seal(l.pc.LocalDescription().SDP)
	if err != nil {
		return
	}
	e.mu.Lock()
	l.answeredKey = key
	l.answeredSealed = sealedAnswer
	e.mu.Unlock()
	e.signal.Answer(peerID, offerID, sealedAnswer)
}

func (e *Engine) renegotiate(peerID string, l *link, offerSDP, key, offerID string) {
	servers, _ := e.iceServers(e.ctx)
	cfg := l.pc.GetConfiguration()
	cfg.ICEServers = servers
	_ = l.pc.SetConfiguration(cfg)

	if err := l.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		e.logf("%s: couldn't read the new route offer (%v)", short(peerID), err)
		return
	}
	answer, err := l.pc.CreateAnswer(nil)
	if err != nil {
		e.logf("%s: couldn't make the new route answer (%v)", short(peerID), err)
		return
	}
	wait := startGather(l.pc, renewOfferWait, e.relayed())
	if err := l.pc.SetLocalDescription(answer); err != nil {
		e.logf("%s: couldn't set the new route answer (%v)", short(peerID), err)
		return
	}
	wait.Wait()

	if !e.ownsLink(peerID, l) || l.pc.LocalDescription() == nil {
		return
	}
	sealed, err := e.room.Seal(l.pc.LocalDescription().SDP)
	if err != nil {
		return
	}
	e.mu.Lock()
	l.answeredKey = key
	l.answeredSealed = sealed
	e.mu.Unlock()
	e.signal.Answer(peerID, offerID, sealed)
}

// --- SDP helpers -----------------------------------------------------------

// sdpAttribute finds one a=<name>:value line. It does not parse the whole SDP
// because only two attributes are needed for deduplication.
func sdpAttribute(sdp, name string) string {
	prefix := "a=" + name + ":"
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// --- gathering -------------------------------------------------------------

type gatherWait struct {
	done chan struct{}
	once sync.Once
}

func (g *gatherWait) finish() {
	g.once.Do(func() { close(g.done) })
}

func (g *gatherWait) Wait() { <-g.done }

// startGather begins watching candidate collection. It stops early on a relay
// candidate when a relay is configured, or 300 ms after the first server
// reflexive candidate otherwise, and always at the timeout.
func startGather(pc *webrtc.PeerConnection, timeout time.Duration, expectRelay bool) *gatherWait {
	g := &gatherWait{done: make(chan struct{})}
	var srflxOnce sync.Once
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			g.finish()
			return
		}
		switch c.Typ {
		case webrtc.ICECandidateTypeRelay:
			g.finish()
		case webrtc.ICECandidateTypeSrflx:
			if !expectRelay {
				srflxOnce.Do(func() {
					time.AfterFunc(routeWait, g.finish)
				})
			}
		}
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		if state == webrtc.ICEGatheringStateComplete {
			g.finish()
		}
	})
	time.AfterFunc(timeout, g.finish)
	return g
}

// --- route statistics ------------------------------------------------------

// waitRoute gives Pion a moment to publish the selected candidate pair.
// Browsers have it ready when the channel opens; Pion can lag a few ticks.
func waitRoute(pc *webrtc.PeerConnection) (route, detail string) {
	for i := 0; i < 15; i++ {
		route, detail = routeStats(pc)
		if detail != "" {
			return route, detail
		}
		time.Sleep(100 * time.Millisecond)
	}
	return route, detail
}

// routeStats reports "direct" or "relay" and a human description like
// "srflx/udp → relay/udp, 42 ms".
func routeStats(pc *webrtc.PeerConnection) (route, detail string) {
	var local, remote *webrtc.ICECandidate
	// Pion exposes the pair directly; the stats report can lag behind.
	if sctp := pc.SCTP(); sctp != nil {
		if dtls := sctp.Transport(); dtls != nil {
			if ice := dtls.ICETransport(); ice != nil {
				if pair, err := ice.GetSelectedCandidatePair(); err == nil && pair != nil {
					local, remote = pair.Local, pair.Remote
				}
			}
		}
	}

	rtt := ""
	report := pc.GetStats()
	for _, st := range report {
		if p, ok := st.(*webrtc.ICECandidatePairStats); ok && p.CurrentRoundTripTime > 0 {
			rtt = fmt.Sprintf(", %d ms", int(p.CurrentRoundTripTime*1000))
			break
		}
	}

	if local == nil || remote == nil {
		return "", ""
	}
	relay := local.Typ == webrtc.ICECandidateTypeRelay ||
		remote.Typ == webrtc.ICECandidateTypeRelay
	if relay {
		route = "relay"
	} else {
		route = "direct"
	}
	detail = fmt.Sprintf("%s/%s → %s/%s%s",
		local.Typ.String(), local.Protocol,
		remote.Typ.String(), remote.Protocol, rtt)
	return route, detail
}
