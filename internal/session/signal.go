package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// DefaultRelayURL is the reference rendezvous/ICE endpoint used by the
// published clients. Point COOP_RELAY_URL at your own Worker for production.
const DefaultRelayURL = "https://coop-mod-loader-relay.jehu-cl.workers.dev"

// PublicTrackers are the WebTorrent trackers the reference client joins.
// They are third-party volunteer services; an empty list is valid when a
// self-hosted relay is used alone.
var PublicTrackers = []string{
	"wss://tracker.openwebtorrent.com",
	"wss://tracker.webtorrent.dev",
	"wss://tracker.novage.com.ua/announce",
	"wss://tracker.peerweb.site/announce",
}

// Offer is one sealed local offer and the id the tracker will echo back.
type Offer struct {
	OfferID   string
	SealedSDP string
}

// SignalEvents are the callbacks the engine receives from the trackers.
type SignalEvents struct {
	OnOffer  func(peerID, sealedSDP, offerID, via string)
	OnAnswer func(peerID, sealedSDP, offerID string)
	OnStatus func(open int)
	Logf     func(format string, args ...any)
}

// Signal maintains one WebSocket per meeting point and speaks the announce
// dialect of WebTorrent trackers.
type Signal struct {
	room     *Room
	peerID   string
	relay    string
	trackers []string
	events   SignalEvents

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	conns  map[string]*trackerConn
	offers []Offer
	closed bool
}

type trackerConn struct {
	url  string
	conn *websocket.Conn

	writeMu sync.Mutex
	open    atomic.Bool
}

// NewSignal builds a client for one room. Relay may be empty to use only the
// public trackers.
func NewSignal(room *Room, peerID, relay string, trackers []string, events SignalEvents) *Signal {
	if peerID == "" {
		peerID = NewPeerID()
	}
	if events.Logf == nil {
		events.Logf = func(string, ...any) {}
	}
	if events.OnStatus == nil {
		events.OnStatus = func(int) {}
	}
	if events.OnOffer == nil {
		events.OnOffer = func(string, string, string, string) {}
	}
	if events.OnAnswer == nil {
		events.OnAnswer = func(string, string, string) {}
	}
	return &Signal{
		room:     room,
		peerID:   peerID,
		relay:    strings.TrimSpace(relay),
		trackers: append([]string(nil), trackers...),
		events:   events,
		conns:    map[string]*trackerConn{},
	}
}

// PeerID returns this client's 20-letter tracker id.
func (s *Signal) PeerID() string { return s.peerID }

// NewPeerID draws 20 uppercase letters.
func NewPeerID() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is fatal for the protocol anyway; fall back to
		// a deterministic-looking id rather than panicking in a UI process.
		for i := range buf {
			buf[i] = byte(i * 7)
		}
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = 'A' + b%26
	}
	return string(out)
}

// Start opens every meeting point. It returns immediately; connection
// failures are retried in the background with 1 s → 10 s backoff.
func (s *Signal) Start(parent context.Context) {
	s.mu.Lock()
	if s.ctx != nil {
		s.mu.Unlock()
		return
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.mu.Unlock()

	targets := make([]string, 0, len(s.trackers)+1)
	if s.relay != "" {
		if u, err := RelayRoomURL(s.relay, s.room.InfoHashHex()); err == nil {
			targets = append(targets, u)
		} else {
			s.events.Logf("relay address is not usable: %v", err)
		}
	}
	for _, t := range s.trackers {
		if strings.TrimSpace(t) != "" {
			targets = append(targets, t)
		}
	}
	for _, target := range targets {
		s.wg.Add(1)
		go s.loop(target)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				s.Announce()
			}
		}
	}()
}

// RelayRoomURL turns https://relay into wss://relay/room/<hex>.
func RelayRoomURL(base, infoHashHex string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", errors.New("relay URL has no host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("relay URL scheme %q is not http(s)", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/room/" + infoHashHex
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (s *Signal) loop(target string) {
	defer s.wg.Done()
	backoff := time.Second
	for {
		if s.stopped() {
			return
		}
		started := time.Now()
		err := s.run(target)
		if s.stopped() {
			return
		}
		_ = err
		if time.Since(started) > 2*time.Second {
			// A socket that lived a while starts its backoff from one second
			// again, like the reference client resetting its retry map.
			backoff = time.Second
		}
		s.events.Logf("meeting point %s dropped, reconnecting in %s", hostOf(target), backoff)
		if !s.sleep(backoff) {
			return
		}
		if backoff < 10*time.Second {
			backoff *= 2
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
		}
	}
}

func (s *Signal) run(target string) error {
	dialCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"User-Agent": {"coop-loader/session"}},
	})
	cancel()
	if err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)

	tc := &trackerConn{url: target, conn: conn}
	tc.open.Store(true)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "closed")
		return context.Canceled
	}
	if old := s.conns[target]; old != nil {
		_ = old.conn.Close(websocket.StatusNormalClosure, "replaced")
	}
	s.conns[target] = tc
	s.mu.Unlock()
	s.status()
	s.events.Logf("meeting point %s is online", hostOf(target))

	s.announceTo(tc)

	for {
		typ, data, err := conn.Read(s.ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		s.handle(data, hostOf(target))
	}

	s.mu.Lock()
	if s.conns[target] == tc {
		delete(s.conns, target)
	}
	tc.open.Store(false)
	s.mu.Unlock()
	_ = conn.Close(websocket.StatusNormalClosure, "")
	s.status()
	return nil
}

func (s *Signal) handle(data []byte, via string) {
	var msg incoming
	if err := json.Unmarshal(data, &msg); err != nil {
		s.events.Logf("discarded a tracker message that was not JSON: %v", err)
		return
	}
	if msg.InfoHash != binString(s.room.InfoHashBinary()) {
		s.events.Logf("tracker message has another info_hash (got %d bytes, want %d)", len(msg.InfoHash), len(s.room.InfoHashBinary()))
		return
	}
	if msg.PeerID == s.peerID || msg.PeerID == "" {
		return
	}
	if msg.Offer != nil && msg.Offer.SDP != "" {
		s.events.OnOffer(msg.PeerID, msg.Offer.SDP, msg.OfferID, via)
	}
	if msg.Answer != nil && msg.Answer.SDP != "" {
		s.events.OnAnswer(msg.PeerID, msg.Answer.SDP, msg.OfferID)
	}
}

// SetOffers replaces the offers announced from now on. When announce is true
// it also sends one update immediately.
func (s *Signal) SetOffers(offers []Offer, announce bool) {
	s.mu.Lock()
	s.offers = append([]Offer(nil), offers...)
	s.mu.Unlock()
	if announce {
		s.Announce()
	}
}

// Announce sends the current offers to every open meeting point.
func (s *Signal) Announce() {
	s.mu.Lock()
	offers := append([]Offer(nil), s.offers...)
	conns := make([]*trackerConn, 0, len(s.conns))
	for _, tc := range s.conns {
		conns = append(conns, tc)
	}
	s.mu.Unlock()

	numwant := len(offers)
	if numwant == 0 {
		numwant = 10
	}
	out := announceOut{
		Action:   "announce",
		InfoHash: binString(s.room.InfoHashBinary()),
		PeerID:   s.peerID,
		NumWant:  numwant,
		Left:     1,
		Event:    "started",
		Offers:   make([]offerOut, 0, len(offers)),
	}
	for _, o := range offers {
		out.Offers = append(out.Offers, offerOut{
			Offer:   sdpOut{Type: "offer", SDP: o.SealedSDP},
			OfferID: o.OfferID,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	for _, tc := range conns {
		s.write(tc, data)
	}
}

// Answer sends one sealed answer back to the peer that offered.
func (s *Signal) Answer(toPeerID, offerID, sealed string) {
	out := answerOut{
		Action:   "announce",
		InfoHash: binString(s.room.InfoHashBinary()),
		PeerID:   s.peerID,
		ToPeerID: toPeerID,
		Answer:   sdpOut{Type: "answer", SDP: sealed},
		OfferID:  offerID,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	s.mu.Lock()
	conns := make([]*trackerConn, 0, len(s.conns))
	for _, tc := range s.conns {
		conns = append(conns, tc)
	}
	s.mu.Unlock()
	for _, tc := range conns {
		s.write(tc, data)
	}
}

func (s *Signal) announceTo(tc *trackerConn) {
	s.mu.Lock()
	offers := append([]Offer(nil), s.offers...)
	s.mu.Unlock()

	numwant := len(offers)
	if numwant == 0 {
		numwant = 10
	}
	out := announceOut{
		Action:   "announce",
		InfoHash: binString(s.room.InfoHashBinary()),
		PeerID:   s.peerID,
		NumWant:  numwant,
		Left:     1,
		Event:    "started",
		Offers:   make([]offerOut, 0, len(offers)),
	}
	for _, o := range offers {
		out.Offers = append(out.Offers, offerOut{
			Offer:   sdpOut{Type: "offer", SDP: o.SealedSDP},
			OfferID: o.OfferID,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	s.write(tc, data)
}

func (s *Signal) write(tc *trackerConn, data []byte) {
	tc.writeMu.Lock()
	defer tc.writeMu.Unlock()
	if !tc.open.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	if err := tc.conn.Write(ctx, websocket.MessageText, data); err != nil {
		// Closing makes the read loop notice and reconnect.
		_ = tc.conn.Close(websocket.StatusInternalError, "write failed")
	}
}

// OpenCount is the number of meeting points currently connected.
func (s *Signal) OpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, tc := range s.conns {
		if tc.open.Load() {
			n++
		}
	}
	return n
}

func (s *Signal) status() { s.events.OnStatus(s.OpenCount()) }

// Close stops every socket and wait loop.
func (s *Signal) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	conns := make([]*trackerConn, 0, len(s.conns))
	for _, tc := range s.conns {
		conns = append(conns, tc)
	}
	s.conns = map[string]*trackerConn{}
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	for _, tc := range conns {
		tc.open.Store(false)
		_ = tc.conn.Close(websocket.StatusNormalClosure, "session ended")
	}
	s.wg.Wait()
}

func (s *Signal) stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Signal) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// --- wire types ------------------------------------------------------------

// binString marshals each code point as \u00xx. Trackers expect the 20 bytes
// of info_hash in a JSON string exactly as JavaScript's String.fromCharCode
// produces them; json's default []byte encoding (Base64) is wrong here.
type binString string

func (b binString) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('"')
	for _, r := range string(b) {
		if r > 0xff {
			return nil, fmt.Errorf("binary string contains %U", r)
		}
		const hex = "0123456789abcdef"
		buf.WriteString(`\u`)
		buf.WriteByte(hex[(r>>12)&0xf])
		buf.WriteByte(hex[(r>>8)&0xf])
		buf.WriteByte(hex[(r>>4)&0xf])
		buf.WriteByte(hex[r&0xf])
	}
	buf.WriteByte('"')
	return buf.Bytes(), nil
}

func (b *binString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for _, r := range s {
		if r > 0xff {
			return fmt.Errorf("info_hash has a wide character %U", r)
		}
	}
	*b = binString(s)
	return nil
}

type sdpOut struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type offerOut struct {
	Offer   sdpOut `json:"offer"`
	OfferID string `json:"offer_id"`
}

type announceOut struct {
	Action     string     `json:"action"`
	InfoHash   binString  `json:"info_hash"`
	PeerID     string     `json:"peer_id"`
	NumWant    int        `json:"numwant"`
	Uploaded   int64      `json:"uploaded"`
	Downloaded int64      `json:"downloaded"`
	Left       int64      `json:"left"`
	Event      string     `json:"event"`
	Offers     []offerOut `json:"offers"`
}

type answerOut struct {
	Action   string    `json:"action"`
	InfoHash binString `json:"info_hash"`
	PeerID   string    `json:"peer_id"`
	ToPeerID string    `json:"to_peer_id"`
	Answer   sdpOut    `json:"answer"`
	OfferID  string    `json:"offer_id"`
}

type sdpIn struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type incoming struct {
	Action   string    `json:"action"`
	InfoHash binString `json:"info_hash"`
	PeerID   string    `json:"peer_id"`
	ToPeerID string    `json:"to_peer_id"`
	Offer    *sdpIn    `json:"offer"`
	Answer   *sdpIn    `json:"answer"`
	OfferID  string    `json:"offer_id"`
}
