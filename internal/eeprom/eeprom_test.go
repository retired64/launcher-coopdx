package eeprom

import (
	"encoding/binary"
	"math/bits"
	"os"
	"path/filepath"
	"testing"
)

func makeSlot(order binary.ByteOrder, exists bool, stars []byte, coins []byte) []byte {
	b := make([]byte, SlotSize)
	b[0] = 1 // cap level
	b[1] = 3 // cap area
	order.PutUint16(b[2:4], 10)
	order.PutUint16(b[4:6], 20)
	order.PutUint16(b[6:8], 30)

	var flags uint32
	if exists {
		flags = 1
	}
	order.PutUint32(b[8:12], flags)
	copy(b[12:37], stars)
	copy(b[37:52], coins)

	if order == binary.BigEndian {
		b[52], b[53] = 'D', 'A'
	} else {
		b[52], b[53] = 'A', 'D'
	}
	var sum uint16
	for _, v := range b[:54] {
		sum += uint16(v)
	}
	order.PutUint16(b[54:56], sum)
	return b
}

func TestParseBigEndian(t *testing.T) {
	stars := make([]byte, 25)
	stars[0] = 0b0111 // three stars
	stars[1] = 0b0001 // one more
	coins := make([]byte, 15)
	coins[0] = 50
	coins[1] = 7

	slot := makeSlot(binary.BigEndian, true, stars, coins)
	file := make([]byte, FileSize)
	copy(file, slot)

	f, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	s := f.Slots[0]
	if !s.Valid || !s.Exists || !s.ChecksumOK {
		t.Fatalf("slot = %+v", s)
	}
	if s.Stars != 4 {
		t.Fatalf("stars = %d, want 4", s.Stars)
	}
	if s.Coins != 57 {
		t.Fatalf("coins = %d, want 57", s.Coins)
	}
	if s.CapLevel != 1 || s.CapArea != 3 || s.CapPos != [3]int16{10, 20, 30} {
		t.Fatalf("cap = %d/%d %v", s.CapLevel, s.CapArea, s.CapPos)
	}
	if s.Signature != "DA" {
		t.Fatalf("signature = %q", s.Signature)
	}
}

func TestParseLittleEndian(t *testing.T) {
	stars := make([]byte, 25)
	stars[0] = 0xff
	slot := makeSlot(binary.LittleEndian, true, stars, make([]byte, 15))
	file := make([]byte, FileSize)
	copy(file, slot)

	f, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if f.Slots[0].Signature != "AD" || f.Slots[0].Stars != bits.OnesCount8(0xff) {
		t.Fatalf("slot = %+v", f.Slots[0])
	}
}

func TestBadChecksumIsInvalid(t *testing.T) {
	slot := makeSlot(binary.BigEndian, true, make([]byte, 25), make([]byte, 15))
	slot[20] ^= 0xff
	file := make([]byte, FileSize)
	copy(file, slot)

	f, err := Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	if f.Slots[0].Valid || f.Slots[0].ChecksumOK {
		t.Fatalf("slot should be invalid: %+v", f.Slots[0])
	}
}

func TestCopyClearRepairAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sm64_save_file.bin")

	stars := make([]byte, 25)
	stars[2] = 0b101
	slotA := makeSlot(binary.BigEndian, true, stars, make([]byte, 15))
	slotB := makeSlot(binary.BigEndian, false, make([]byte, 25), make([]byte, 15))
	file := make([]byte, FileSize)
	copy(file[0:], slotA)
	copy(file[SlotSize:], slotB)
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.CopySlot(0, 2); err != nil {
		t.Fatal(err)
	}
	if f.Slots[2].Stars != 2 || !f.Slots[2].Exists {
		t.Fatalf("copy = %+v", f.Slots[2])
	}
	if err := f.ClearSlot(1); err != nil {
		t.Fatal(err)
	}
	if f.Slots[1].Valid {
		t.Fatal("cleared slot should be invalid")
	}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(dir, BackupName)
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Slots[2].Stars != 2 || again.Slots[1].Valid {
		t.Fatalf("reloaded = %+v", again.Slots)
	}

	// Repair a deliberately broken checksum.
	broken := again.Data()
	broken[3] ^= 0xff
	bf, _ := Parse(broken)
	if bf.Slots[0].ChecksumOK {
		t.Fatal("expected broken checksum")
	}
	if err := bf.Repair(0); err != nil {
		t.Fatal(err)
	}
	if !bf.Slots[0].ChecksumOK {
		t.Fatal("repair did not fix the checksum")
	}
}
