// Package eeprom reads and writes Super Mario 64's 512-byte save file.
//
// Layout (verified against the format used by sm64coopdx):
//
//	0..511   four save slots of 112 bytes
//	slot:    header 56 bytes + 56 bytes of extra data
//	header:  cap level (1) | cap area (1) | cap position (3x int16)
//	         flags (uint32) | stars (25 bytes) | coins (15 bytes)
//	         signature "DA"/"AD" (2 bytes) | checksum (uint16) = sum of first 54 bytes
//
// "DA" means big-endian fields, "AD" means little-endian. Both appear in the
// wild, so the loader detects the order per slot instead of assuming one.
package eeprom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"

	"github.com/retired64/coop-loader/internal/fsutil"
)

const (
	// FileSize is the exact size of sm64_save_file.bin.
	FileSize = 512
	// SlotCount is the number of save slots in the file.
	SlotCount = 4
	// SlotSize is the number of bytes per slot.
	SlotSize = 112
	// HeaderSize is the checksummed part of a slot.
	HeaderSize = 56
	// BackupName is created next to the save before the first change.
	BackupName = "sm64_save_file.modloader-backup.bin"
)

// ErrSize means the file is not a 512-byte SM64 save.
var ErrSize = fmt.Errorf("a save file is exactly %d bytes", FileSize)

// ErrSlot means a slot has no valid signature/checksum.
var ErrSlot = errors.New("that slot does not contain a save")

// Slot is the decoded summary of one save you can show to a player.
type Slot struct {
	Index      int      `json:"index"`
	Valid      bool     `json:"valid"`
	Exists     bool     `json:"exists"`
	ChecksumOK bool     `json:"checksumOk"`
	Signature  string   `json:"signature"`
	Stars      int      `json:"stars"`
	Coins      int      `json:"coins"`
	CapLevel   int      `json:"capLevel"`
	CapArea    int      `json:"capArea"`
	CapPos     [3]int16 `json:"capPos"`
	Flags      uint32   `json:"flags"`
}

// File is an in-memory save file.
type File struct {
	data  []byte
	Slots [SlotCount]Slot
}

// Parse validates and decodes a save file.
func Parse(data []byte) (*File, error) {
	if len(data) != FileSize {
		return nil, ErrSize
	}
	f := &File{data: append([]byte(nil), data...)}
	for i := 0; i < SlotCount; i++ {
		f.Slots[i] = parseSlot(i, f.data[i*SlotSize:(i+1)*SlotSize])
	}
	return f, nil
}

// Load reads a save file from disk.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Data returns a copy of the raw bytes.
func (f *File) Data() []byte {
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out
}

// RawSlot returns a copy of one 112-byte slot.
func (f *File) RawSlot(i int) ([]byte, error) {
	if i < 0 || i >= SlotCount {
		return nil, fmt.Errorf("slot %d is not between 0 and %d", i, SlotCount-1)
	}
	out := make([]byte, SlotSize)
	copy(out, f.data[i*SlotSize:(i+1)*SlotSize])
	return out, nil
}

// SetSlot copies a valid 112-byte slot into the file.
func (f *File) SetSlot(i int, raw []byte) error {
	if i < 0 || i >= SlotCount {
		return fmt.Errorf("slot %d is not between 0 and %d", i, SlotCount-1)
	}
	if len(raw) != SlotSize {
		return fmt.Errorf("a save slot is %d bytes", SlotSize)
	}
	slot := parseSlot(i, raw)
	if !slot.Valid {
		return ErrSlot
	}
	copy(f.data[i*SlotSize:(i+1)*SlotSize], raw)
	f.Slots[i] = slot
	return nil
}

// CopySlot duplicates one slot onto another. The old destination is kept in
// memory only; call Save to persist.
func (f *File) CopySlot(from, to int) error {
	if from == to {
		return nil
	}
	if !f.Slots[from].Valid {
		return ErrSlot
	}
	raw, err := f.RawSlot(from)
	if err != nil {
		return err
	}
	return f.SetSlot(to, raw)
}

// ClearSlot erases a slot. The game shows it as empty.
func (f *File) ClearSlot(i int) error {
	if i < 0 || i >= SlotCount {
		return fmt.Errorf("slot %d is not between 0 and %d", i, SlotCount-1)
	}
	for j := i * SlotSize; j < (i+1)*SlotSize; j++ {
		f.data[j] = 0
	}
	f.Slots[i] = Slot{Index: i}
	return nil
}

// Repair recomputes the checksum of a slot, keeping its data.
func (f *File) Repair(i int) error {
	if i < 0 || i >= SlotCount {
		return fmt.Errorf("slot %d is not between 0 and %d", i, SlotCount-1)
	}
	start := i * SlotSize
	slot := f.data[start : start+SlotSize]
	order, _ := byteOrder(slot)
	if order == nil {
		return ErrSlot
	}
	order.PutUint16(slot[54:56], checksum(slot))
	f.Slots[i] = parseSlot(i, slot)
	return nil
}

// Save writes the file, keeping a one-time backup of the original next to it.
func (f *File) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	backup := filepath.Join(filepath.Dir(path), BackupName)
	if fsutil.Exists(path) && !fsutil.Exists(backup) {
		if err := fsutil.CopyFile(path, backup, 0o644); err != nil {
			return err
		}
	}
	return fsutil.WriteFileAtomic(path, f.data, 0o644)
}

func parseSlot(index int, b []byte) Slot {
	s := Slot{Index: index}
	order, signature := byteOrder(b)
	s.Signature = signature
	if order == nil {
		return s
	}
	s.ChecksumOK = checksum(b) == order.Uint16(b[54:56])
	if !s.ChecksumOK {
		return s
	}
	s.Valid = true
	s.Flags = order.Uint32(b[8:12])
	s.Exists = s.Flags&1 != 0
	s.CapLevel = int(b[0])
	s.CapArea = int(b[1])
	s.CapPos = [3]int16{
		int16(order.Uint16(b[2:4])),
		int16(order.Uint16(b[4:6])),
		int16(order.Uint16(b[6:8])),
	}
	s.Stars = popcount(b[12:37])
	s.Coins = sumBytes(b[37:52])
	return s
}

func byteOrder(b []byte) (binary.ByteOrder, string) {
	switch {
	case b[52] == 'D' && b[53] == 'A':
		return binary.BigEndian, "DA"
	case b[52] == 'A' && b[53] == 'D':
		return binary.LittleEndian, "AD"
	default:
		return nil, ""
	}
}

func checksum(b []byte) uint16 {
	var sum uint16
	for _, v := range b[:54] {
		sum += uint16(v)
	}
	return sum
}

func popcount(b []byte) int {
	n := 0
	for _, v := range b {
		n += bits.OnesCount8(v)
	}
	return n
}

func sumBytes(b []byte) int {
	n := 0
	for _, v := range b {
		n += int(v)
	}
	return n
}
