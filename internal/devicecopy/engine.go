package devicecopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/retired64/coop-loader/internal/session"
)

// ErrNoReview means the receiver has not reached the review screen yet.
var ErrNoReview = errors.New("There is nothing to copy right now.")

type inbound struct {
	data []byte
	text bool
}

// Engine is one copy/v1 session. It owns the trackers, the WebRTC link and the
// staging directory; the app owns the files.
type Engine struct {
	cfg  Config
	room *session.Room

	mu       sync.Mutex
	state    State
	peer     *DeviceInfo
	route    string
	manifest *Manifest
	plan     *Plan
	review   *Review
	dc       *webrtc.DataChannel
	pc       *webrtc.PeerConnection
	staging  string
	closing  bool

	ctx    context.Context
	cancel context.CancelFunc

	signal *session.Signal

	offerIDs    map[string]bool
	offerStop   chan struct{}
	offerOnce   sync.Once
	peerID      string
	fingerprint string
	answered    bool

	pairReady chan struct{}
	pairErr   chan error
	pairOnce  sync.Once
	pairFailO sync.Once

	in       chan inbound
	acceptCh chan Choice

	closeOnce sync.Once
	logs      *ringLog
}

// New builds a stopped engine.
func New(cfg Config) *Engine {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.KDFIterations <= 0 {
		cfg.KDFIterations = session.DefaultIterations
	}
	if cfg.Device.Name == "" {
		cfg.Device.Name = "This computer"
	}
	return &Engine{
		cfg:       cfg,
		offerIDs:  map[string]bool{},
		pairReady: make(chan struct{}),
		pairErr:   make(chan error, 1),
		in:        make(chan inbound, 1024),
		acceptCh:  make(chan Choice, 1),
		logs:      newRingLog(400),
	}
}

// Device returns this side's device info.
func (e *Engine) Device() DeviceInfo { return e.cfg.Device }

// Start derives the room code and begins looking for the other device.
func (e *Engine) Start(parent context.Context) (State, error) {
	if parent == nil {
		parent = context.Background()
	}
	if e.cfg.Role != RoleSend && e.cfg.Role != RoleGet {
		return State{}, errors.New("the copy role must be send or get")
	}
	code := strings.TrimSpace(e.cfg.Code)
	if code == "" {
		if e.cfg.Role != RoleSend {
			return State{}, errors.New("Type the code your other device shows.")
		}
		generated, err := session.Generate()
		if err != nil {
			return State{}, err
		}
		code = generated
	}
	normalized, err := session.Normalize(code)
	if err != nil {
		return State{}, errors.New("That doesn't look like a code. It has 8 letters and numbers, like K7QD-9XPA.")
	}
	start := time.Now()
	room, err := session.DeriveWithSalt(normalized, CopySalt, e.cfg.KDFIterations)
	if err != nil {
		return State{}, err
	}
	e.room = room
	e.ctx, e.cancel = context.WithCancel(parent)

	relay := e.cfg.RelayURL
	if relay == "" && !e.cfg.DisableRelay {
		relay = session.DefaultRelayURL
	}
	trackers := e.cfg.Trackers
	if len(trackers) == 0 && !e.cfg.DisableTrackers {
		trackers = session.PublicTrackers
	}

	e.mu.Lock()
	e.state = State{Role: e.cfg.Role, Code: normalized, Phase: PhaseStarting}
	e.mu.Unlock()
	e.logf("copy room ready in %d ms (%s)", time.Since(start).Milliseconds(), room.Describe())

	e.signal = session.NewSignal(room, "", relay, trackers, session.SignalEvents{
		OnOffer: func(peerID, sealed, offerID, via string) {
			if e.cfg.Role == RoleSend {
				go e.handleOffer(peerID, sealed, offerID, via)
			}
		},
		OnAnswer: func(peerID, sealed, offerID string) {
			if e.cfg.Role == RoleGet {
				go e.handleAnswer(peerID, sealed, offerID)
			}
		},
		OnStatus: e.onTrackers,
		Logf:     e.logf,
	})
	e.signal.Start(e.ctx)
	go e.run()

	return e.State(), nil
}

// State returns a copy of the public state.
func (e *Engine) State() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// Logs returns the recent log, newest last.
func (e *Engine) Logs() []string { return e.logs.Lines() }

// Accept tells the engine which parts to copy. It is only valid after the
// receiver reaches the review phase.
func (e *Engine) Accept(choice Choice) error {
	e.mu.Lock()
	phase := e.state.Phase
	e.mu.Unlock()
	if phase != PhaseReview {
		return ErrNoReview
	}
	select {
	case e.acceptCh <- choice:
		return nil
	case <-e.ctx.Done():
		return ErrStopped
	}
}

// Stop ends the copy and releases the sockets.
func (e *Engine) Stop() {
	e.mu.Lock()
	switch e.state.Phase {
	case PhaseDone, PhaseFailed, PhaseStopped:
		e.mu.Unlock()
		e.cleanup()
		return
	}
	e.state.Phase = PhaseStopped
	e.state.Review = nil
	e.state.Progress = nil
	e.state.Message = nil
	dc := e.dc
	e.mu.Unlock()

	e.sendStop("The other device stopped the copy.")
	_ = dc
	if e.cancel != nil {
		e.cancel()
	}
	e.cleanup()
	e.publish()
}

func (e *Engine) run() {
	var err error
	defer func() {
		if err != nil {
			e.fail(err)
		}
		e.cleanup()
		e.publish()
	}()
	if e.cfg.Role == RoleGet {
		err = e.pairGet()
	} else {
		err = e.pairSend()
	}
	if err != nil {
		return
	}
	if err = e.handshake(); err != nil {
		return
	}
	if e.cfg.Role == RoleGet {
		err = e.receiveSide()
	} else {
		err = e.sendSide()
	}
}

// --- pairing ---------------------------------------------------------------

func (e *Engine) pairGet() error {
	servers, expectRelay := e.iceServers()
	pc, err := e.newPeer(servers)
	if err != nil {
		return fmt.Errorf("Couldn't start: %v", err)
	}
	e.mu.Lock()
	e.pc = pc
	e.mu.Unlock()

	ordered := true
	dc, err := pc.CreateDataChannel("copy", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return fmt.Errorf("Couldn't start: %v", err)
	}
	e.wire(dc)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		e.logf("connection %s", state)
		if state == webrtc.PeerConnectionStateFailed {
			e.pairFail(errors.New("Couldn't connect to your other device, not even through the relay."))
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	wait := session.StartGather(pc, GatherTimeout, expectRelay)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	wait.Wait()
	if pc.LocalDescription() == nil {
		return errors.New("Couldn't start the copy.")
	}
	sealed, err := e.room.Seal(pc.LocalDescription().SDP)
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.offerIDs = map[string]bool{}
	offers := make([]session.Offer, 0, OfferCount)
	for i := 0; i < OfferCount; i++ {
		id := session.NewPeerID()
		e.offerIDs[id] = true
		offers = append(offers, session.Offer{OfferID: id, SealedSDP: sealed})
	}
	e.mu.Unlock()

	stop := make(chan struct{})
	e.mu.Lock()
	e.offerStop = stop
	e.mu.Unlock()
	e.signal.SetOffers(offers, true)
	go func() {
		t := time.NewTicker(OfferEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				e.signal.Announce()
			case <-stop:
				return
			case <-e.ctx.Done():
				return
			}
		}
	}()

	return e.waitPaired()
}

func (e *Engine) pairSend() error {
	e.mu.Lock()
	// The signal was already started in Start; offers drive this side.
	e.mu.Unlock()
	return e.waitPaired()
}

func (e *Engine) handleAnswer(peerID, sealed, offerID string) {
	e.mu.Lock()
	if e.cfg.Role != RoleGet || e.answered || e.state.Phase == PhaseStopped || e.state.Phase == PhaseFailed {
		e.mu.Unlock()
		return
	}
	if !e.offerIDs[offerID] {
		e.mu.Unlock()
		return
	}
	pc := e.pc
	e.mu.Unlock()
	if pc == nil || pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		return
	}
	sdp, ok := e.room.Open(sealed)
	if !ok {
		return
	}
	e.mu.Lock()
	if e.answered || pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
		e.mu.Unlock()
		return
	}
	e.answered = true
	e.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		e.pairFail(fmt.Errorf("Couldn't connect: %v", err))
		return
	}
	e.stopOffers()
	e.signal.SetOffers(nil, false)
	e.logf("the other device answered")
}

func (e *Engine) handleOffer(peerID, sealed, offerID, via string) {
	sdp, ok := e.room.Open(sealed)
	if !ok {
		e.logf("an offer could not be opened")
		return
	}
	fingerprint := session.SDPAttribute(sdp, "fingerprint")
	if fingerprint == "" {
		return
	}

	e.mu.Lock()
	if e.cfg.Role != RoleSend || e.state.Phase == PhaseDone || e.state.Phase == PhaseStopped || e.state.Phase == PhaseFailed {
		e.mu.Unlock()
		return
	}
	if e.peerID != "" && e.peerID != peerID {
		e.mu.Unlock()
		return
	}
	if e.fingerprint == fingerprint {
		e.mu.Unlock()
		return
	}
	e.peerID = peerID
	e.fingerprint = fingerprint
	old := e.pc
	e.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	servers, expectRelay := e.iceServers()
	pc, err := e.newPeer(servers)
	if err != nil {
		e.logf("couldn't create a connection: %v", err)
		return
	}
	e.mu.Lock()
	e.pc = pc
	e.mu.Unlock()
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		e.logf("connection %s", state)
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "copy" || !dc.Ordered() {
			_ = dc.Close()
			return
		}
		e.wire(dc)
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		e.logf("couldn't read the offer: %v", err)
		e.resetPeerPC(pc)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		e.logf("couldn't make an answer: %v", err)
		e.resetPeerPC(pc)
		return
	}
	wait := session.StartGather(pc, GatherTimeout, expectRelay)
	if err := pc.SetLocalDescription(answer); err != nil {
		e.logf("couldn't set the answer: %v", err)
		e.resetPeerPC(pc)
		return
	}
	wait.Wait()
	if pc.LocalDescription() == nil {
		e.resetPeerPC(pc)
		return
	}
	sealedAnswer, err := e.room.Seal(pc.LocalDescription().SDP)
	if err != nil {
		e.resetPeerPC(pc)
		return
	}
	e.signal.Answer(peerID, offerID, sealedAnswer)
	e.logf("answered the other device via %s", via)
}

func (e *Engine) resetPeerPC(pc *webrtc.PeerConnection) {
	e.mu.Lock()
	if e.pc == pc {
		e.pc = nil
	}
	e.mu.Unlock()
	_ = pc.Close()
}

func (e *Engine) wire(dc *webrtc.DataChannel) {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		_ = dc.Close()
		return
	}
	e.dc = dc
	e.mu.Unlock()

	dc.OnOpen(func() {
		e.markPaired()
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		data := append([]byte(nil), msg.Data...)
		select {
		case e.in <- inbound{data: data, text: msg.IsString}:
		case <-e.ctx.Done():
		}
	})
	dc.OnClose(func() {
		e.channelClosed()
	})
}

func (e *Engine) markPaired() {
	e.pairOnce.Do(func() { close(e.pairReady) })
}

func (e *Engine) pairFail(err error) {
	e.pairFailO.Do(func() {
		select {
		case e.pairErr <- err:
		default:
		}
	})
}

func (e *Engine) waitPaired() error {
	timer := time.NewTimer(PairTimeout)
	defer timer.Stop()
	select {
	case <-e.pairReady:
		return nil
	case err := <-e.pairErr:
		return err
	case <-timer.C:
		return errors.New("Couldn't find your other device. Check the code, and that it still shows it.")
	case <-e.ctx.Done():
		return e.ctxError()
	}
}

func (e *Engine) channelClosed() {
	e.mu.Lock()
	closing := e.closing
	phase := e.state.Phase
	e.mu.Unlock()
	if closing || phase == PhaseDone || phase == PhaseStopped || phase == PhaseFailed {
		return
	}
	e.fail(ErrLost)
}

func (e *Engine) stopOffers() {
	e.mu.Lock()
	stop := e.offerStop
	e.mu.Unlock()
	e.offerOnce.Do(func() {
		if stop != nil {
			close(stop)
		}
	})
}

// --- handshake -------------------------------------------------------------

func (e *Engine) handshake() error {
	if err := e.sendJSON(map[string]any{
		"t":      "hello",
		"v":      ProtocolVersion,
		"device": e.cfg.Device,
	}); err != nil {
		return err
	}
	msg, err := e.next()
	if err != nil {
		return err
	}
	if !msg.text {
		return errors.New("The other device isn't Coop Mod Loader.")
	}
	var hello struct {
		T      string     `json:"t"`
		V      int        `json:"v"`
		Device DeviceInfo `json:"device"`
	}
	if err := json.Unmarshal(msg.data, &hello); err != nil || hello.T != "hello" {
		return errors.New("The other device isn't Coop Mod Loader.")
	}
	peer := sanitizePeer(hello.Device)
	if hello.V != ProtocolVersion {
		if hello.V > ProtocolVersion {
			return errors.New("Update Coop Mod Loader on this device: the other one is newer.")
		}
		return fmt.Errorf("Update Coop Mod Loader on %s: it is older than this one.", peer.Name)
	}
	e.mu.Lock()
	e.peer = &peer
	e.state.Peer = &peer
	e.state.Phase = PhaseConnected
	e.mu.Unlock()
	e.publish()
	e.logf("connected to %s", peer.Name)

	route := e.resolveRoute()
	if route != "" {
		e.mu.Lock()
		e.route = route
		e.state.Route = &route
		e.mu.Unlock()
		e.publish()
	}
	return nil
}

func sanitizePeer(device DeviceInfo) DeviceInfo {
	name := SanitizeControl(device.Name, 64)
	if name == "" {
		name = "Your other device"
	}
	kind := device.Kind
	switch kind {
	case "windows", "linux", "android":
	default:
		kind = "windows"
	}
	return DeviceInfo{Name: name, Kind: kind}
}

// --- sender ----------------------------------------------------------------

func (e *Engine) sendSide() error {
	if e.cfg.BuildManifest == nil {
		return errors.New("There is nothing to copy right now.")
	}
	src, err := e.cfg.BuildManifest()
	if err != nil {
		return err
	}
	if src == nil {
		return errors.New("There is nothing to copy right now.")
	}
	if len(src.Text) > ManifestMaxBytes {
		return ErrManifestBig
	}
	chunks := ManifestChunks(src.Text)
	if len(chunks) > ManifestChunkLimit() {
		return ErrManifestBig
	}
	for i, text := range chunks {
		if err := e.waitBuffered(); err != nil {
			return err
		}
		if err := e.sendJSON(map[string]any{"t": "manifest", "i": i, "n": len(chunks), "text": text}); err != nil {
			return err
		}
	}
	raw, err := e.expect("want")
	if err != nil {
		return err
	}
	var want struct {
		Files []int `json:"files"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		return ErrMixed
	}
	sizes := make([]int64, len(src.Files))
	for i := range src.Files {
		sizes[i] = src.Files[i].Size
	}
	if err := ValidateWant(want.Files, sizes); err != nil {
		return err
	}
	var total int64
	for _, index := range want.Files {
		total += sizes[index]
	}
	e.setProgress(0, total)
	e.setPhase(PhaseCopying)

	var acked atomic.Int64
	control := make(chan error, 1)
	go e.readControl(control, &acked)

	var sent int64
	last := time.Now()
	for _, index := range want.Files {
		size := sizes[index]
		for offset := int64(0); offset < size || (offset == 0 && size == 0); offset += FileChunk {
			if err := e.waitFlow(sent, acked.Load()); err != nil {
				return err
			}
			length := int64(FileChunk)
			if size-offset < length {
				length = size - offset
			}
			data, err := src.Read(index, offset, int(length))
			if err != nil {
				return ErrFileChanged
			}
			if int64(len(data)) != length {
				return ErrFileChanged
			}
			if err := e.sendFrame(index, offset, data); err != nil {
				return err
			}
			sent += length
			if time.Since(last) >= ProgressTick {
				e.setProgress(sent, total)
				last = time.Now()
			}
			if size == 0 {
				break
			}
		}
	}
	e.setProgress(sent, total)
	if err := <-control; err != nil {
		return err
	}
	e.mu.Lock()
	e.state.Phase = PhaseDone
	e.state.Progress = &Progress{Done: sent, Total: total}
	e.state.Message = nil
	e.mu.Unlock()
	e.publish()
	e.logf("sent %d bytes", sent)
	return nil
}

func (e *Engine) readControl(out chan<- error, acked *atomic.Int64) {
	for {
		msg, err := e.next()
		if err != nil {
			out <- err
			return
		}
		if !msg.text {
			out <- ErrMixed
			return
		}
		var env struct {
			T     string `json:"t"`
			Bytes int64  `json:"bytes"`
			Why   string `json:"why"`
		}
		if err := json.Unmarshal(msg.data, &env); err != nil {
			out <- ErrMixed
			return
		}
		switch env.T {
		case "ack":
			if env.Bytes > acked.Load() {
				acked.Store(env.Bytes)
			}
		case "done":
			out <- nil
			return
		case "stop":
			out <- errors.New(stopReason(env.Why))
			return
		default:
			out <- ErrMixed
			return
		}
	}
}

func (e *Engine) waitBuffered() error {
	for {
		dc := e.dataChannel()
		if dc == nil {
			return ErrLost
		}
		if dc.BufferedAmount() <= MaxBuffered {
			return nil
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-e.ctx.Done():
			return e.ctxError()
		}
	}
}

func (e *Engine) waitFlow(sent, acked int64) error {
	for sent-acked > MaxInFlight {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-e.ctx.Done():
			return e.ctxError()
		}
	}
	for {
		dc := e.dataChannel()
		if dc == nil {
			return ErrLost
		}
		if dc.BufferedAmount() <= MaxBuffered {
			return nil
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-e.ctx.Done():
			return e.ctxError()
		}
	}
}

// --- receiver --------------------------------------------------------------

func (e *Engine) receiveSide() error {
	if e.cfg.Review == nil || e.cfg.Plan == nil || e.cfg.Apply == nil {
		return errors.New("The copy is not configured.")
	}
	root, err := os.MkdirTemp(e.cfg.StagingRoot, "copy-")
	if err != nil {
		return fmt.Errorf("Couldn't prepare the copy: %v", err)
	}
	e.mu.Lock()
	e.staging = root
	e.mu.Unlock()
	inDir := filepath.Join(root, "in")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		return err
	}

	var text strings.Builder
	for i := 0; ; i++ {
		raw, err := e.expect("manifest")
		if err != nil {
			return err
		}
		var part struct {
			I    int    `json:"i"`
			N    int    `json:"n"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &part); err != nil || part.I != i || part.N < 1 {
			return ErrMixed
		}
		text.WriteString(part.Text)
		if text.Len() > ManifestMaxBytes || part.N > ManifestChunkLimit() || i+1 > part.N {
			return ErrManifestBig
		}
		if i+1 >= part.N {
			break
		}
	}
	manifest, err := ParseManifest(text.String())
	if err != nil {
		return err
	}
	route := e.currentRoute()
	review, err := e.cfg.Review(manifest, route)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.manifest = manifest
	e.review = &review
	e.plan = nil
	e.state.Review = &review
	e.state.Phase = PhaseReview
	e.state.Progress = nil
	e.mu.Unlock()
	e.publish()
	e.logf("manifest received: %d mods, %d files", len(manifest.Mods), len(manifest.Flatten()))

	var choice Choice
	select {
	case choice = <-e.acceptCh:
	case <-e.ctx.Done():
		return e.ctxError()
	}
	plan, err := e.cfg.Plan(manifest, choice, route)
	if err != nil {
		return err
	}
	if plan == nil {
		return errors.New("There is nothing to copy right now.")
	}
	if err := ValidateWant(plan.Want, manifest.Sizes()); err != nil {
		return err
	}
	e.mu.Lock()
	e.plan = plan
	e.state.Review = nil
	e.state.Phase = PhaseCopying
	e.state.Progress = &Progress{Total: plan.Bytes}
	e.mu.Unlock()
	e.publish()

	if err := e.sendJSON(map[string]any{"t": "want", "files": plan.Want}); err != nil {
		return err
	}

	flat := manifest.Flatten()
	var received int64
	var acked int64
	last := time.Now()
	for _, index := range plan.Want {
		file := flat[index].Ref
		partPath := filepath.Join(inDir, strconv.Itoa(index))
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		h := sha256.New()
		got := int64(0)
		chunkIndex := 0
		for {
			msg, err := e.next()
			if err != nil {
				f.Close()
				return err
			}
			if msg.text {
				var env struct {
					T   string `json:"t"`
					Why string `json:"why"`
				}
				if json.Unmarshal(msg.data, &env) == nil && env.T == "stop" {
					f.Close()
					return errors.New(stopReason(env.Why))
				}
				f.Close()
				return ErrMixed
			}
			frame, ok := DecodeFrame(msg.data)
			if !ok || frame.File != index || frame.Offset != got || got+int64(len(frame.Data)) > file.Size {
				f.Close()
				return ErrMixed
			}
			if chunkIndex == 0 && isLuaName(file.Rel) && len(frame.Data) > 0 && frame.Data[0] == 0x1b && !file.Code {
				f.Close()
				return ErrCompiledSwap
			}
			if len(frame.Data) > 0 {
				if _, err := f.Write(frame.Data); err != nil {
					f.Close()
					return err
				}
				h.Write(frame.Data)
			}
			got += int64(len(frame.Data))
			received += int64(len(frame.Data))
			chunkIndex++
			if received-acked >= AckEvery {
				if err := e.sendJSON(map[string]any{"t": "ack", "bytes": received}); err != nil {
					f.Close()
					return err
				}
				acked = received
			}
			if time.Since(last) >= ProgressTick {
				e.setProgress(received, plan.Bytes)
				last = time.Now()
			}
			if got >= file.Size {
				break
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != file.SHA256 {
			return ErrFileDamaged
		}
	}
	e.setProgress(received, plan.Bytes)

	note, err := e.cfg.Apply(manifest, plan, root)
	if err != nil {
		return err
	}
	if err := e.sendJSON(map[string]any{"t": "done", "note": nil}); err != nil {
		return err
	}
	e.mu.Lock()
	e.state.Phase = PhaseDone
	e.state.Progress = &Progress{Done: received, Total: plan.Bytes}
	if note != "" {
		e.state.Message = &note
	} else {
		e.state.Message = nil
	}
	e.mu.Unlock()
	e.publish()
	e.logf("copy finished")
	return nil
}

// --- helpers ---------------------------------------------------------------

func (e *Engine) newPeer(servers []webrtc.ICEServer) (*webrtc.PeerConnection, error) {
	setting := webrtc.SettingEngine{}
	setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(setting))
	policy := webrtc.ICETransportPolicyAll
	if e.cfg.RelayOnly {
		policy = webrtc.ICETransportPolicyRelay
	}
	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers:         servers,
		ICETransportPolicy: policy,
	})
}

func (e *Engine) iceServers() ([]webrtc.ICEServer, bool) {
	base := make([]webrtc.ICEServer, 0, 2)
	for _, stun := range session.DefaultSTUNServers() {
		base = append(base, webrtc.ICEServer{URLs: []string{stun}})
	}
	relay := e.cfg.RelayURL
	if relay == "" && !e.cfg.DisableRelay {
		relay = session.DefaultRelayURL
	}
	if relay == "" {
		return base, false
	}
	fetched, err := session.FetchICEServers(e.ctx, relay)
	if err != nil {
		e.logf("relay logins unavailable right now; direct routes only (%v)", err)
		return base, false
	}
	servers := append(base, fetched...)
	expectRelay := false
	for _, server := range fetched {
		for _, raw := range server.URLs {
			lower := strings.ToLower(raw)
			if strings.HasPrefix(lower, "turn:") || strings.HasPrefix(lower, "turns:") {
				expectRelay = true
			}
		}
	}
	return servers, expectRelay
}

func (e *Engine) resolveRoute() string {
	deadline := time.Now().Add(RouteWait)
	e.mu.Lock()
	pc := e.pc
	e.mu.Unlock()
	for {
		route, detail := session.RouteStats(pc)
		if route != "" {
			if detail != "" {
				e.logf("route %s", detail)
			}
			return route
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *Engine) currentRoute() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.route
}

func (e *Engine) dataChannel() *webrtc.DataChannel {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dc
}

func (e *Engine) next() (inbound, error) {
	select {
	case msg := <-e.in:
		return msg, nil
	case <-e.ctx.Done():
		return inbound{}, e.ctxError()
	}
}

func (e *Engine) expect(kind string) (json.RawMessage, error) {
	for {
		msg, err := e.next()
		if err != nil {
			return nil, err
		}
		if !msg.text {
			return nil, ErrMixed
		}
		var env struct {
			T   string `json:"t"`
			Why string `json:"why"`
		}
		if err := json.Unmarshal(msg.data, &env); err != nil {
			// The reference client ignores a malformed text frame.
			continue
		}
		if env.T == "stop" {
			return nil, errors.New(stopReason(env.Why))
		}
		if env.T != kind {
			return nil, ErrMixed
		}
		return json.RawMessage(msg.data), nil
	}
}

func (e *Engine) sendJSON(value any) error {
	dc := e.dataChannel()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrLost
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := dc.SendText(string(data)); err != nil {
		return ErrLost
	}
	return nil
}

func (e *Engine) sendFrame(index int, offset int64, data []byte) error {
	dc := e.dataChannel()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrLost
	}
	if err := dc.Send(EncodeFrame(index, offset, data)); err != nil {
		return ErrLost
	}
	return nil
}

func (e *Engine) sendStop(reason string) {
	dc := e.dataChannel()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	payload, _ := json.Marshal(map[string]any{"t": "stop", "why": SanitizeControl(reason, 200)})
	_ = dc.SendText(string(payload))
}

func (e *Engine) ctxError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Phase == PhaseStopped {
		return ErrStopped
	}
	if e.state.Phase == PhaseFailed && e.state.Message != nil {
		return errors.New(*e.state.Message)
	}
	return ErrLost
}

func (e *Engine) fail(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	switch e.state.Phase {
	case PhaseDone, PhaseFailed, PhaseStopped:
		e.mu.Unlock()
		return
	}
	msg := err.Error()
	if errors.Is(err, ErrStopped) {
		msg = ""
	}
	e.state.Phase = PhaseFailed
	e.state.Review = nil
	if msg != "" {
		e.state.Message = &msg
	} else {
		e.state.Message = nil
	}
	e.mu.Unlock()

	if msg != "" {
		e.sendStop(msg)
	}
	if e.cancel != nil {
		e.cancel()
	}
	e.publish()
}

func (e *Engine) cleanup() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closing = true
		staging := e.staging
		e.staging = ""
		e.mu.Unlock()
		if e.cancel != nil {
			e.cancel()
		}
		if e.signal != nil {
			e.signal.Close()
		}
		if dc := e.dataChannel(); dc != nil {
			_ = dc.Close()
		}
		e.mu.Lock()
		pc := e.pc
		e.pc = nil
		e.mu.Unlock()
		if pc != nil {
			_ = pc.Close()
		}
		if staging != "" {
			_ = os.RemoveAll(staging)
		}
	})
}

func (e *Engine) onTrackers(open int) {
	e.mu.Lock()
	e.state.Trackers = open
	if open > 0 && e.state.Phase == PhaseStarting {
		e.state.Phase = PhaseWaiting
	}
	e.mu.Unlock()
	e.publish()
}

func (e *Engine) setPhase(phase Phase) {
	e.mu.Lock()
	if e.state.Phase == PhaseStopped || e.state.Phase == PhaseFailed || e.state.Phase == PhaseDone {
		e.mu.Unlock()
		return
	}
	e.state.Phase = phase
	e.mu.Unlock()
	e.publish()
}

func (e *Engine) setProgress(done, total int64) {
	e.mu.Lock()
	e.state.Progress = &Progress{Done: done, Total: total}
	e.mu.Unlock()
	e.publish()
}

func (e *Engine) snapshotLocked() State {
	st := e.state
	if st.Review != nil {
		review := *st.Review
		st.Review = &review
	}
	if st.Progress != nil {
		progress := *st.Progress
		st.Progress = &progress
	}
	if st.Message != nil {
		msg := *st.Message
		st.Message = &msg
	}
	if st.Peer != nil {
		peer := *st.Peer
		st.Peer = &peer
	}
	if st.Route != nil {
		route := *st.Route
		st.Route = &route
	}
	return st
}

func (e *Engine) publish() {
	e.mu.Lock()
	st := e.snapshotLocked()
	e.mu.Unlock()
	if e.cfg.OnState != nil {
		e.cfg.OnState(st)
	}
}

func (e *Engine) logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	e.logs.Add(line)
	e.cfg.Logf("%s", line)
}

func stopReason(reason string) string {
	reason = SanitizeControl(reason, 200)
	if reason == "" {
		return "The other device stopped the copy."
	}
	return reason
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
