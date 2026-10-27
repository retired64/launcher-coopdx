// Package devicecopy implements the Coop Mod Loader copy protocol (copy/v1).
//
// It is a clean-room port of the observed desktop client: two devices derive
// the same room from a code, meet on the same WebTorrent trackers, open an
// ordered/reliable DataChannel named "copy", exchange a JSON manifest, and
// stream raw file chunks with SHA-256 verification.
//
// The package does not know how mods or saves are stored. The app package
// supplies the manifest, reviews it, and applies the received files.
package devicecopy

import "time"

// Role is the side of the copy. The receiver ("get") creates the WebRTC
// offer; the sender ("send") answers. This is the opposite of the game room.
type Role string

const (
	RoleSend Role = "send"
	RoleGet  Role = "get"
)

// Phase is the public state of a copy.
type Phase string

const (
	PhaseStarting  Phase = "starting"
	PhaseWaiting   Phase = "waiting"
	PhaseConnected Phase = "connected"
	PhaseReview    Phase = "review"
	PhaseCopying   Phase = "copying"
	PhaseDone      Phase = "done"
	PhaseFailed    Phase = "failed"
	PhaseStopped   Phase = "stopped"
)

// DeviceInfo identifies one loader installation.
type DeviceInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Progress is the byte counter shown while copying.
type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// ModsReview summarises what the manifest would change.
type ModsReview struct {
	Added     []string `json:"added"`
	Updated   []string `json:"updated"`
	On        int      `json:"on"`
	Bytes     int64    `json:"bytes"`
	TurnedOff []string `json:"turnedOff"`
	Missing   []string `json:"missing"`
	Skipped   []string `json:"skipped"`
	Compiled  []string `json:"compiled"`
}

// LookReview is the player name, character and colours in the manifest.
type LookReview struct {
	Name    string            `json:"name"`
	Model   int               `json:"model"`
	Colors  map[string]string `json:"colors"`
	Presets []string          `json:"presets"`
}

// SaveSlotReview compares one save slot on both devices.
type SaveSlotReview struct {
	Slot  int  `json:"slot"`
	Stars *int `json:"stars"`
	Here  *int `json:"here"`
}

// SavesReview summarises the save file and the mod saves.
type SavesReview struct {
	Slots    []SaveSlotReview `json:"slots"`
	ModSaves int              `json:"modSaves"`
}

// Review is what the receiver sees before accepting.
type Review struct {
	From  DeviceInfo  `json:"from"`
	Mods  ModsReview  `json:"mods"`
	Look  LookReview  `json:"look"`
	Saves SavesReview `json:"saves"`
}

// State is the public copy state, mirrored by the HTTP API and the UI.
type State struct {
	Role     Role        `json:"role"`
	Code     string      `json:"code"`
	Phase    Phase       `json:"phase"`
	Trackers int         `json:"trackers"`
	Peer     *DeviceInfo `json:"peer"`
	Route    *string     `json:"route"`
	Review   *Review     `json:"review"`
	Progress *Progress   `json:"progress"`
	Message  *string     `json:"message"`
}

// Choice is what the receiver ticks before accepting.
type Choice struct {
	Mods  bool `json:"mods"`
	Look  bool `json:"look"`
	Saves bool `json:"saves"`
}

// FileRef is one file the sender will stream. It is returned by BuildManifest
// together with a reader closure, so the engine never guesses paths.
type FileRef struct {
	Size int64
}

// Source is the sender's manifest and file reader.
type Source struct {
	// Text is the compact JSON manifest.
	Text string
	// Files is the flattened file list in manifest order.
	Files []FileRef
	// Read returns exactly length bytes at offset, or an error when the
	// file changed on disk.
	Read func(index int, offset int64, length int) ([]byte, error)
}

// Config wires the copy engine to the rest of the loader.
type Config struct {
	Role     Role
	Code     string
	Device   DeviceInfo
	RelayURL string
	// DisableRelay and DisableTrackers mirror COOP_RELAY_URL and
	// COOP_NO_TRACKERS in the session engine.
	DisableRelay    bool
	Trackers        []string
	DisableTrackers bool
	// RelayOnly forces TURN candidates, like the reference test switch.
	RelayOnly bool
	// KDFIterations is only lowered by tests.
	KDFIterations int
	// StagingRoot is where received parts are written. It must be on the
	// same filesystem as the final game data for atomic renames.
	StagingRoot string

	Logf    func(format string, args ...any)
	OnState func(State)

	BuildManifest func() (*Source, error)
	Review        func(manifest *Manifest, route string) (Review, error)
	Plan          func(manifest *Manifest, choice Choice, route string) (*Plan, error)
	Apply         func(manifest *Manifest, plan *Plan, stagingDir string) (string, error)
}

// Timing and size limits copied from the reference client.
const (
	ProtocolVersion = 1
	CopySalt        = "coop-mod-loader/copy/v1"

	ManifestMaxBytes = 4 << 20
	ManifestChunk    = 60000
	FileChunk        = 64 << 10
	FrameHeader      = 12

	MaxInFlight  = 8 << 20
	AckEvery     = 1 << 20
	MaxBuffered  = 4 << 20
	ProgressTick = 250 * time.Millisecond

	PairTimeout    = 90 * time.Second
	OfferCount     = 6
	OfferEvery     = 2 * time.Second
	GatherTimeout  = 5 * time.Second
	RouteWait      = 1500 * time.Millisecond
	GatherRelayGap = 300 * time.Millisecond

	MaxMods     = 2000
	MaxFiles    = 20000
	MaxFileSize = 1024 * 1024 * 1024
	MaxTotal    = 16 * 1024 * 1024 * 1024
	MaxPresets  = 300
	MaxModSaves = 2000
	MaxModSave  = 1024 * 1024

	// RelayBudget is the total mod bytes copied when the route is relay.
	RelayBudget = 500_000_000
)
