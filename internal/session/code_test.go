package session

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateAndNormalize(t *testing.T) {
	for i := 0; i < 50; i++ {
		code, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 9 || code[4] != '-' {
			t.Fatalf("code %q is not ABCD-1234 shaped", code)
		}
		again, err := Normalize(code)
		if err != nil || again != code {
			t.Fatalf("Normalize(%q) = %q, %v", code, again, err)
		}
	}
}

func TestNormalizeCrockford(t *testing.T) {
	cases := map[string]string{
		"abcd-1234":   "ABCD-1234",
		" K7QD 9XPA ": "K7QD-9XPA",
		"k7qd_9xpa":   "K7QD-9XPA",
		"oiluabcd":    "011V-ABCD",
		"oooooooo":    "0000-0000",
		"ABCD-1234":   "ABCD-1234",
	}
	for in, want := range cases {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"", "ABCD", "ABCD-123", "ABCD-12345", "ABCD-123!"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) should fail", bad)
		}
	}
}

// The vector was produced independently with Python's hashlib.pbkdf2_hmac.
func TestDeriveKnownVector(t *testing.T) {
	room, err := Derive("ABCD-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(room.InfoHash[:]); got != "42568f8e201a83e8de74ed167204a49509f4b6e9" {
		t.Fatalf("info hash = %s", got)
	}
	if got := hex.EncodeToString(room.Key); got != "f167b2c34eda1e9f866fa7c2e00784b370a563f75faaf7f526b7fff3506e66b4" {
		t.Fatalf("key = %s", got)
	}
	if len(room.InfoHashHex()) != 40 {
		t.Fatalf("hex = %q", room.InfoHashHex())
	}
}

func TestSealOpen(t *testing.T) {
	room, err := DeriveWith("K7QD-9XPA", 1000)
	if err != nil {
		t.Fatal(err)
	}
	sdp := "v=0\r\na=fingerprint:sha-256 AA:BB\r\n"
	sealed, err := room.Seal(sdp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "fingerprint") {
		t.Fatal("sealed SDP is not encrypted")
	}
	got, ok := room.Open(sealed)
	if !ok || got != sdp {
		t.Fatalf("Open = %q, %v", got, ok)
	}

	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if _, ok := room.Open(base64.StdEncoding.EncodeToString(raw)); ok {
		t.Fatal("tampered ciphertext must not open")
	}
	if _, ok := room.Open("not base64!"); ok {
		t.Fatal("bad base64 must not open")
	}

	other, _ := DeriveWith("ZZZZ-ZZZZ", 1000)
	if _, ok := other.Open(sealed); ok {
		t.Fatal("another room must not open it")
	}
}

func TestInfoHashJSONIsBinaryString(t *testing.T) {
	room, err := DeriveWith("ABCD-1234", 1000)
	if err != nil {
		t.Fatal(err)
	}
	msg := struct {
		Hash binString `json:"info_hash"`
	}{Hash: binString(room.InfoHashBinary())}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	// Every byte is emitted as a \u00xx escape, so the JSON is plain ASCII.
	for _, b := range data {
		if b > 0x7f {
			t.Fatalf("JSON is not ASCII: %q", data)
		}
	}
	var back struct {
		Hash binString `json:"info_hash"`
	}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Hash != msg.Hash {
		t.Fatalf("round trip changed info_hash")
	}
}

func TestRelayRoomURL(t *testing.T) {
	got, err := RelayRoomURL("https://relay.example/base/", "aabb")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://relay.example/base/room/aabb" {
		t.Fatalf("url = %q", got)
	}
	got, _ = RelayRoomURL("http://127.0.0.1:8080", "00")
	if got != "ws://127.0.0.1:8080/room/00" {
		t.Fatalf("url = %q", got)
	}
	if _, err := RelayRoomURL("ftp://x", "00"); err == nil {
		t.Fatal("ftp should fail")
	}
}
