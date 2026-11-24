package cnfrt

import (
	"bytes"
	"testing"
)

// FuzzParseHeader checks that header parsing never panics and that every
// accepted directory describes in-bounds, aligned, ordered sections.
func FuzzParseHeader(f *testing.F) {
	img, _ := AppendImage(nil, testHeader(), testSections())
	f.Add(img)
	f.Add(img[:HeaderSize])
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, mode := range []VerifyMode{VerifyTrustedHeader, VerifyChecksum} {
			h, infos, err := ParseHeader(data, mode, 0)
			if err != nil {
				continue
			}
			if h.TotalSize != uint64(len(data)) {
				t.Fatal("size mismatch accepted")
			}
			prev := uint64(0)
			for _, in := range infos {
				if in.Off < prev || in.Off+in.Len > uint64(len(data)) || in.Off%SectionAlign != 0 {
					t.Fatalf("bad section accepted: %+v", in)
				}
				prev = in.Off + in.Len
				_ = SectionData(data, in)
			}
		}
	})
}

// FuzzParseCompactImage checks that accepted compact images describe
// sections inside the image, in order and aligned.
func FuzzParseCompactImage(f *testing.F) {
	img, _, _ := AppendCompactImage(nil, testHeader(), testSections())
	f.Add(img)
	f.Add(img[:CompactHeaderSize])
	empty, _, _ := AppendCompactImage(nil, testHeader(), nil)
	f.Add(empty)
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, mode := range []VerifyMode{VerifyTrustedHeader, VerifyChecksum} {
			h, infos, err := ParseCompactImage(data, mode, 0)
			if err != nil {
				continue
			}
			if h.TotalSize != uint64(len(data)) {
				t.Fatal("size mismatch accepted")
			}
			prev := uint64(CompactHeaderSize + len(infos)*CompactDirEntrySize)
			for _, in := range infos {
				if in.Off < prev || in.Off+in.Len > uint64(len(data)) || in.Off%CompactAlign != 0 {
					t.Fatalf("bad section accepted: %+v", in)
				}
				prev = in.Off + in.Len
				_ = SectionData(data, in)
			}
		}
	})
}

// FuzzDecodeExt checks that ext tables never cause panics.
func FuzzDecodeExt(f *testing.F) {
	f.Add(EncodeExt([]LocEntry{{Kind: LocFixed, Name: "X", Offset: 60}}, nil))
	f.Add([]byte{1, 0, 0, 0, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		DecodeExt(data)
		for i := range uint32(4) {
			ExtLoc(string(data), i)
			ExtZone(string(data), uint16(i))
		}
	})
}

// FuzzFrozenMapSections reassembles FrozenMaps from arbitrary storage:
// validated maps must support every operation without panicking, and
// unvalidated maps must at least terminate lookups.
func FuzzFrozenMapSections(f *testing.F) {
	m, _ := NewFrozenMap(map[string]string{"alpha": "1", "beta": "22", "gamma": "333"}, nil)
	sec := m.Sections()
	f.Add(sec.Ctrl, AppendSliceLE(nil, sec.Slots), sec.Data, sec.NumGroups, sec.Len)
	f.Fuzz(func(t *testing.T, ctrl, slotBytes []byte, data string, groups, n uint32) {
		slots := make([]FrozenSlot, len(slotBytes)/16)
		for i := range slots {
			b := slotBytes[i*16:]
			slots[i] = FrozenSlot{le32(b), le32(b[4:]), le32(b[8:]), le32(b[12:])}
		}
		s := FrozenSections{Seed: DefaultSeed, NumGroups: groups, Len: n, Ctrl: bytes.Clone(ctrl), Slots: slots, Data: data}
		if vm, err := FrozenMapFromSections[string, string](s, nil, true); err == nil {
			for k, v := range vm.All() {
				if g, ok := vm.Get(k); !ok || (g != v && !vm.Has(k)) {
					t.Fatalf("validated lookup of %q failed", k)
				}
			}
		}
		if um, err := FrozenMapFromSections[string, string](s, nil, false); err == nil {
			um.Has("alpha")
		}
	})
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
