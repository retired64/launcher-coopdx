package session

// Mode is how this loader takes part in a room.
type Mode string

const (
	// ModeHost owns the room code, answers offers and gives every friend a
	// private loopback port into the game.
	ModeHost Mode = "host"
	// ModeJoin types a friend's code, makes offers and connects its single
	// loopback port to the game.
	ModeJoin Mode = "join"
)

// Phase is the public state of a session.
type Phase string

const (
	PhaseStarting   Phase = "starting"
	PhaseWaiting    Phase = "waiting"
	PhaseConnecting Phase = "connecting"
	PhaseConnected  Phase = "connected"
	PhaseFailed     Phase = "failed"
	PhaseStopped    Phase = "stopped"
)

// Friend is one remote player as the UI sees them.
type Friend struct {
	ID           string  `json:"id"`
	Route        *string `json:"route"`
	Reconnecting bool    `json:"reconnecting"`
	Game         *bool   `json:"game"`
}

// State is what the UI polls.
type State struct {
	Mode     Mode     `json:"mode"`
	Code     string   `json:"code"`
	Phase    Phase    `json:"phase"`
	Friends  []Friend `json:"friends"`
	Trackers int      `json:"trackers"`
	Message  *string  `json:"message"`
	GameIn   bool     `json:"gameIn"`
	Port     int      `json:"port,omitempty"`
	Relayed  bool     `json:"relayed,omitempty"`
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
