// Package session implements the Coop Mod Loader online session engine.
//
// The engine is a clean-room port of the observed desktop client:
//
//   - a 40-bit Crockford room code is stretched with PBKDF2-SHA-256 into a
//     20-byte room id and a 32-byte AES-256-GCM key;
//   - WebRTC offers and answers travel through WebTorrent-compatible
//     trackers, sealed with that key;
//   - game datagrams travel over an unreliable, unordered DataChannel named
//     "game";
//   - a loopback UDP bridge connects that DataChannel to sm64coopdx on
//     ../../../127.0.0.1:7777, one socket per friend on the host.
//
// Nothing in this package knows about the loader's files or UI. The app
// package owns the process lifetime.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Alphabet is Crockford's base32 without I, L, O and U.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RoomSalt is the domain separator used by the room protocol. A future copy
// protocol uses "coop-mod-loader/copy/v1".
const RoomSalt = "coop-mod-loader/room/v2"

// DefaultIterations is the PBKDF2 work factor of the v2 room protocol.
// Lowering it breaks compatibility, so only tests pass another value.
const DefaultIterations = 600000

// ErrCode means the text is not a room code.
var ErrCode = errors.New("that is not a room code: it has 8 letters and numbers, like K7QD-9XPA")

// Generate returns a fresh code in the display form ABCD-1234.
//
// Eight independent bytes are drawn and reduced modulo 32. Because 256 is a
// multiple of 32 the result is uniform, matching the reference client.
func Generate() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b [8]byte
	for i, v := range buf {
		b[i] = Alphabet[int(v)%len(Alphabet)]
	}
	return string(b[:4]) + "-" + string(b[4:]), nil
}

// Normalize validates and canonicalises a user-typed code.
//
// It uppercases, removes spaces and hyphens, maps the Crockford confusables
// O->0, I/L->1, U->V, and returns the ABCD-1234 form.
func Normalize(raw string) (string, error) {
	clean := Raw(raw)
	if len(clean) != 8 {
		return "", ErrCode
	}
	for i := 0; i < len(clean); i++ {
		if !strings.ContainsRune(Alphabet, rune(clean[i])) {
			return "", ErrCode
		}
	}
	return clean[:4] + "-" + clean[4:], nil
}

// Raw returns the eight symbols without the middle hyphen. It does not
// validate; call Normalize for that.
func Raw(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch r {
		case ' ', '\t', '\r', '\n', '-', '_':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		case 'U':
			r = 'V'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Room is the material derived from one code.
type Room struct {
	Code     string
	InfoHash [20]byte
	Key      []byte
	gcm      cipher.AEAD
}

// Derive stretches the code with the protocol's default work factor.
func Derive(code string) (*Room, error) {
	return DeriveWith(code, DefaultIterations)
}

// DeriveWith is Derive with an explicit PBKDF2 iteration count. Production
// code must use Derive; tests use a small count to stay fast.
func DeriveWith(code string, iterations int) (*Room, error) {
	return DeriveWithSalt(code, RoomSalt, iterations)
}

// DeriveWithSalt is the generic derivation used by the room and copy
// protocols. They differ only in the salt; the code, work factor and key
// layout are identical, so a copy room can never collide with a game room.
func DeriveWithSalt(code, salt string, iterations int) (*Room, error) {
	display, err := Normalize(code)
	if err != nil {
		return nil, err
	}
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	dk, err := pbkdf2.Key(sha256.New, Raw(display), []byte(salt), iterations, 52)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dk[20:52])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	room := &Room{Code: display, Key: append([]byte(nil), dk[20:52]...), gcm: gcm}
	copy(room.InfoHash[:], dk[:20])
	return room, nil
}

// InfoHashBinary returns the 20 bytes as a Go string of runes 0..255.
//
// Trackers carry info_hash as a JSON string whose code points are the raw
// bytes. Mapping each byte to one rune preserves that on the wire instead of
// mangling values above 0x7f as invalid UTF-8.
func (r *Room) InfoHashBinary() string {
	runes := make([]rune, len(r.InfoHash))
	for i, b := range r.InfoHash {
		runes[i] = rune(b)
	}
	return string(runes)
}

// InfoHashHex is the form used in the relay's WebSocket path.
func (r *Room) InfoHashHex() string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(r.InfoHash)*2)
	for i, b := range r.InfoHash {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

// Seal encrypts an SDP with AES-256-GCM and returns Base64(nonce || ct+tag).
func (r *Room) Seal(plain string) (string, error) {
	nonce := make([]byte, r.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := r.gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a sealed SDP. It returns ok=false for anything that was not
// produced by this room; a hostile tracker must not be able to crash a peer.
func (r *Room) Open(sealed string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < r.gcm.NonceSize() {
		return "", false
	}
	nonce, body := raw[:r.gcm.NonceSize()], raw[r.gcm.NonceSize():]
	plain, err := r.gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// Describe is a short, secret-free description for logs.
func (r *Room) Describe() string {
	return fmt.Sprintf("room %x…", r.InfoHash[:4])
}
