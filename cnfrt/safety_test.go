package cnfrt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type failingMarshaler struct {
	fail bool
	data string
}

func (f failingMarshaler) MarshalBinary() ([]byte, error) {
	if f.fail {
		return nil, errors.New("marshal failed")
	}
	return []byte(f.data), nil
}

func (f *failingMarshaler) UnmarshalBinary(b []byte) error {
	f.data = string(b)
	return nil
}

func TestLocationCacheClonesName(t *testing.T) {
	fixedLocs.m.Store(nil)
	var freed atomic.Bool
	func() {
		big := make([]byte, 4<<20)
		copy(big, "CLONED_TZ")
		runtime.AddCleanup(&big[0], func(p *atomic.Bool) { p.Store(true) }, &freed)
		sub := UnsafeSealBytes(big[:9])
		loc := FixedLocation(sub, 7200)
		if loc.String() != "CLONED_TZ" {
			t.Fatalf("got %q", loc.String())
		}
	}()
	for range 20 {
		runtime.GC()
		if freed.Load() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !freed.Load() {
		t.Fatal("FixedLocation retained backing buffer of zone name substring")
	}
	// Cached entry remains readable after the original buffer was collected.
	if loc := FixedLocation("CLONED_TZ", 7200); loc.String() != "CLONED_TZ" {
		t.Fatalf("cached entry corrupted: %q", loc.String())
	}
}

func TestCustomTZDataFallbackAndOffsetOverflow(t *testing.T) {
	// Minimal TZif v1 payload defining a single fixed offset (+03:45) named "CUST".
	tzif := make([]byte, 44+6+5)
	copy(tzif[0:4], "TZif")
	binary.BigEndian.PutUint32(tzif[32:36], 0) // timecnt
	binary.BigEndian.PutUint32(tzif[36:40], 1) // typecnt
	binary.BigEndian.PutUint32(tzif[40:44], 5) // charcnt
	binary.BigEndian.PutUint32(tzif[44:48], uint32(3*3600+45*60))
	tzif[48] = 0 // isdst
	tzif[49] = 0 // abbrind
	copy(tzif[50:], "CUST\x00")

	loc, err := time.LoadLocationFromTZData("Custom/NotInTZDB", tzif)
	if err != nil {
		t.Fatalf("LoadLocationFromTZData: %v", err)
	}
	in := time.Date(2026, 7, 1, 10, 0, 0, 0, loc)
	var lt LocTable
	ft := lt.Encode(in)
	if ft.Kind() != LocFixed && ft.Kind() != LocIndexed {
		t.Fatalf("expected fixed/indexed fallback for unknown tzdb name, got kind %d", ft.Kind())
	}
	out := DecodeTime(ft, lt.Locations())
	if !out.Equal(in) {
		t.Fatalf("instant mismatch: got %v want %v", out, in)
	}
	_, o1 := in.Zone()
	_, o2 := out.Zone()
	if o1 != o2 {
		t.Fatalf("offset mismatch: got %d want %d", o2, o1)
	}
	// Multi-transition custom TZif (not in system tzdb) must also fall back
	// from LocIANA to LocFixed without panicking.
	tzif2 := make([]byte, 44+4+1+12+10)
	copy(tzif2[0:4], "TZif")
	binary.BigEndian.PutUint32(tzif2[32:36], 1)  // timecnt
	binary.BigEndian.PutUint32(tzif2[36:40], 2)  // typecnt
	binary.BigEndian.PutUint32(tzif2[40:44], 10) // charcnt
	binary.BigEndian.PutUint32(tzif2[44:48], 0)  // transition at unix 0
	tzif2[48] = 1                                // type index 1 after unix 0
	binary.BigEndian.PutUint32(tzif2[49:53], uint32(2*3600))
	tzif2[53] = 0
	tzif2[54] = 0
	binary.BigEndian.PutUint32(tzif2[55:59], uint32(3*3600))
	tzif2[59] = 1
	tzif2[60] = 5
	copy(tzif2[61:], "STD1\x00DST2\x00")
	loc2, err := time.LoadLocationFromTZData("Custom/MultiZone", tzif2)
	if err != nil {
		t.Fatalf("LoadLocationFromTZData multi: %v", err)
	}
	in2 := time.Date(2026, 7, 1, 10, 0, 0, 0, loc2)
	var ltMulti LocTable
	ft2 := ltMulti.Encode(in2)
	if ft2.Kind() != LocFixed && ft2.Kind() != LocIndexed {
		t.Fatalf("expected fixed/indexed fallback for multi-zone TZData, got kind %d", ft2.Kind())
	}
	out2 := DecodeTime(ft2, ltMulti.Locations())
	n2a, o2a := in2.Zone()
	n2b, o2b := out2.Zone()
	if !out2.Equal(in2) || n2a != n2b || o2a != o2b {
		t.Fatalf("multi-zone round trip: got (%s,%d) want (%s,%d)", n2b, o2b, n2a, o2a)
	}

	// Verify int32 offset overflow fails cleanly via Abort instead of truncating.
	if ^uint(0)>>32 != 0 {
		v := int64(1) << 33
		bigOff := int(v)
		bad := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("HUGE", bigOff))
		var encodeErr error
		func() {
			defer func() { Recover(recover(), &encodeErr) }()
			var lt2 LocTable
			_ = lt2.Encode(bad)
		}()
		if !errors.Is(encodeErr, ErrCapacityExceeded) {
			t.Fatalf("huge FixedZone offset: got %v, want ErrCapacityExceeded", encodeErr)
		}
	}
}

func TestLocTableFixedZoneMetaCache(t *testing.T) {
	tz := time.FixedZone("IST", 5*3600+30*60)
	var lt LocTable
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, tz)
	var first FlatTime
	for i := range 20_000 {
		ft := lt.Encode(base.Add(time.Duration(i) * time.Second))
		if i == 0 {
			first = ft
		} else if ft.Meta != first.Meta {
			t.Fatalf("meta mismatch at %d: %#x vs %#x", i, ft.Meta, first.Meta)
		}
	}
	locs := lt.Locations()
	got := DecodeTime(first, locs)
	if !got.Equal(base) || got.Location().String() != "IST" {
		t.Fatalf("got %v (%s)", got, got.Location())
	}
}

func TestFrozenMapNetIPAndTimeLossless(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	fixed := time.FixedZone("BRT", -3*3600)
	t1 := time.Date(2026, 7, 15, 14, 30, 0, 123456789, berlin)
	t2 := time.Date(2026, 1, 15, 14, 30, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 1, 8, 0, 0, 0, fixed)

	a1 := netip.MustParseAddr("10.0.0.1")
	a2 := netip.MustParseAddr("fe80::1%eth0")
	a3 := netip.Addr{}
	m, err := NewFrozenMap(map[netip.Addr]time.Time{a1: t1, a2: t2, a3: t3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got1, ok := m.Get(a1)
	if !ok || !got1.Equal(t1) || got1.Location().String() != "Europe/Berlin" {
		t.Fatalf("got1 = %v (%s), want %v (%s)", got1, got1.Location(), t1, t1.Location())
	}
	got2, ok := m.Get(a2)
	if !ok || !got2.Equal(t2) || got2.Location() != time.UTC {
		t.Fatalf("got2 = %v (%s)", got2, got2.Location())
	}
	got3, ok := m.Get(a3)
	if !ok || !got3.Equal(t3) || got3.Location().String() != "BRT" {
		t.Fatalf("got3 = %v (%s)", got3, got3.Location())
	}

	pm, err := NewFrozenMap(map[netip.Prefix]string{
		netip.MustParsePrefix("192.168.1.0/24"): "lan",
		netip.MustParsePrefix("2001:db8::/32"):  "doc",
	}, nil)
	if err != nil || pm.Validate() != nil {
		t.Fatalf("prefix map: %v", err)
	}
	if v, ok := pm.Get(netip.MustParsePrefix("192.168.1.0/24")); !ok || v != "lan" {
		t.Fatalf("prefix get: %q %v", v, ok)
	}

	apm, err := NewFrozenMap(map[netip.AddrPort]uint64{
		netip.MustParseAddrPort("[fe80::1%en0]:443"): 1,
		netip.MustParseAddrPort("127.0.0.1:8080"):    2,
	}, nil)
	if err != nil || apm.Validate() != nil {
		t.Fatalf("addrport map: %v", err)
	}
	if v, ok := apm.Get(netip.MustParseAddrPort("[fe80::1%en0]:443")); !ok || v != 1 {
		t.Fatalf("addrport get: %d %v", v, ok)
	}
}

func TestFrozenMapBorrowAndCorruptIteration(t *testing.T) {
	m, err := NewFrozenMap(map[string]string{"k": "value"}, &MapOptions[string]{CloneStrings: true})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if !m.Borrow("k", func(raw string) { got = strings.Clone(raw) }) {
		t.Fatal("Borrow missed existing key")
	}
	if got != "value" {
		t.Fatalf("Borrow got %q", got)
	}
	if m.Borrow("missing", func(string) { t.Fatal("callback invoked on miss") }) {
		t.Fatal("Borrow hit missing key")
	}

	// Corrupt control byte (>0x7f) and out-of-bounds spans must be skipped
	// without panicking during All and RawEntries in unvalidated mode.
	sec := m.Sections()
	sec.Ctrl = append([]byte(nil), sec.Ctrl...)
	sec.Slots = append([]FrozenSlot(nil), sec.Slots...)
	sec.Ctrl[0] = 0x99 // invalid control byte
	sec.Ctrl[1] = 0x01
	sec.Slots[1] = FrozenSlot{KeyOff: 0xfffffff0, KeyLen: 64, ValOff: 0, ValLen: 1}
	corrupt, err := FrozenMapFromSections[string, string](sec, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for range corrupt.All() {
	}
	for range corrupt.RawEntries() {
	}
}

func TestFrozenMapValidateLinearBounds(t *testing.T) {
	// Default seeds are non-zero and randomized unless DeterministicSeed is set.
	m1, _ := NewFrozenMap(map[string]int{"a": 1}, nil)
	m2, _ := NewFrozenMap(map[string]int{"a": 1}, nil)
	if m1.Seed() == 0 || m2.Seed() == 0 || m1.Seed() == m2.Seed() {
		t.Fatalf("expected distinct non-zero default seeds: %#x vs %#x", m1.Seed(), m2.Seed())
	}
	d1, _ := NewFrozenMap(map[string]int{"a": 1}, &MapOptions[int]{DeterministicSeed: true})
	d2, _ := NewFrozenMap(map[string]int{"a": 1}, &MapOptions[int]{DeterministicSeed: true})
	if d1.Seed() != DefaultSeed || d2.Seed() != DefaultSeed {
		t.Fatalf("DeterministicSeed did not select DefaultSeed: %#x %#x", d1.Seed(), d2.Seed())
	}

	// Overlapping spans exceeding data size must be rejected in O(N).
	blob := strings.Repeat("x", 1024)
	const n = 200
	groups := uint32(64)
	ctrl := make([]byte, groups*groupSlots)
	for i := range ctrl {
		ctrl[i] = ctrlEmpty
	}
	slots := make([]FrozenSlot, groups*groupSlots)
	for i := range n {
		k := blob[:500+i]
		h := HashString(k, DefaultSeed)
		g := (h >> 7) & uint64(groups-1)
		for probe := uint64(0); ; probe++ {
			base := int(g) * groupSlots
			placed := false
			for j := range groupSlots {
				if ctrl[base+j] == ctrlEmpty {
					ctrl[base+j] = byte(h & 0x7f)
					slots[base+j] = FrozenSlot{KeyOff: 0, KeyLen: uint32(len(k)), ValOff: 0, ValLen: 1}
					placed = true
					break
				}
			}
			if placed {
				break
			}
			g = (g + probe + 1) & uint64(groups-1)
		}
	}
	sec := FrozenSections{Seed: DefaultSeed, NumGroups: groups, Len: n, Ctrl: ctrl, Slots: slots, Data: blob}
	if _, err := FrozenMapFromSections[string, string](sec, nil, true); !errors.Is(err, ErrBoundsViolation) {
		t.Fatalf("overlapping spans accepted: %v", err)
	}
}

func TestFrozenMapFallibleCodecAndZeroAllocLookups(t *testing.T) {
	b, err := NewFrozenBuilder[string, failingMarshaler](4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Add("ok", failingMarshaler{data: "v"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Add("bad", failingMarshaler{fail: true}); err == nil {
		t.Fatal("expected error from failing BinaryMarshaler")
	}
	fm, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if fm.Len() != 1 {
		t.Fatalf("failed Add leaked entry: Len=%d", fm.Len())
	}

	// Verify 0 allocs/op for uint64, uint32, and string keys.
	m64, err := NewFrozenMap(map[uint64]uint64{1: 10, 2: 20, 3: 30}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoAllocs(t, "FrozenMap[uint64,uint64].Get", func() {
		v, ok := m64.Get(2)
		if !ok || v != 20 {
			t.Fatal("mismatch")
		}
	})
	m32, err := NewFrozenMap(map[uint32]uint32{1: 10, 2: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoAllocs(t, "FrozenMap[uint32,uint32].Get", func() {
		v, ok := m32.Get(2)
		if !ok || v != 20 {
			t.Fatal("mismatch")
		}
	})
	ms, err := NewFrozenMap(map[string]string{"alpha": "beta"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoAllocs(t, "FrozenMap[string,string].Get", func() {
		v, ok := ms.Get("alpha")
		if !ok || v != "beta" {
			t.Fatal("mismatch")
		}
	})
}

func TestSlabMapDeadSlabReclaimAndSafeAll(t *testing.T) {
	m, err := NewSlabMapSize[uint64, string](4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("a", 200)
	for round := range 100 {
		base := uint64(round * 200)
		for i := range uint64(200) {
			if err := m.Put(base+i, payload); err != nil {
				t.Fatal(err)
			}
		}
		for i := range uint64(200) {
			m.Delete(base + i)
		}
	}
	m.Compact()
	st := m.Stats()
	if st.Len != 0 || st.DeadBytes != 0 {
		t.Fatalf("after 100 Put/Delete cycles + Compact: %+v, want Len=0 DeadBytes=0", st)
	}

	// Mutating the map while iterating with All must not deadlock.
	for i := range uint64(50) {
		_ = m.Put(i, "v")
	}
	seen := 0
	for k := range m.All() {
		seen++
		m.Delete(k)
		if k < 1000 {
			_ = m.Put(k+1000, "new")
		}
	}
	if seen < 50 {
		t.Fatalf("All visited %d entries, want at least 50", seen)
	}

	// FallibleCodec errors return cleanly from Put without panicking.
	fm, err := NewSlabMap[string, failingMarshaler](nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Put("bad", failingMarshaler{fail: true}); err == nil {
		t.Fatal("expected error from failing BinaryMarshaler in SlabMap.Put")
	}
}

func TestSlabMapSmallPutZeroAllocs(t *testing.T) {
	m, err := NewSlabMapSize[uint64, uint64](64<<10, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-warm the shard index and slab.
	for i := range uint64(64) {
		if err := m.Put(i, i); err != nil {
			t.Fatal(err)
		}
	}
	k := uint64(7)
	assertNoAllocs(t, "SlabMap[uint64,uint64].Put", func() {
		if err := m.Put(k, 99); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHashUint32MatchesHash(t *testing.T) {
	var buf [4]byte
	for _, seed := range []uint64{0, 1, DefaultSeed, RandomSeed()} {
		for _, v := range []uint32{0, 1, 42, 0xdeadbeef, 0xffffffff} {
			binary.LittleEndian.PutUint32(buf[:], v)
			if got, want := HashUint32(v, seed), Hash(buf[:], seed); got != want {
				t.Fatalf("HashUint32(%#x, %#x) = %#x, want %#x", v, seed, got, want)
			}
		}
	}
}

type failSeekWriter struct {
	memFile
	failSeek bool
	err      error
}

func (w *failSeekWriter) Seek(off int64, whence int) (int64, error) {
	if w.failSeek {
		return 0, w.err
	}
	return w.memFile.Seek(off, whence)
}

func TestChunkedWriterCloseRetainsError(t *testing.T) {
	want := io.ErrShortWrite
	fw := &failSeekWriter{err: want}
	cw, err := NewChunkedWriter(fw, Header{SchemaHash: 1}, []SectionSpec{
		{Kind: SectionStrBlob, ElemAlign: 1, Count: 5, Len: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	fw.failSeek = true
	err1 := cw.Close()
	if !errors.Is(err1, want) {
		t.Fatalf("first Close = %v, want %v", err1, want)
	}
	err2 := cw.Close()
	if !errors.Is(err2, want) {
		t.Fatalf("second Close = %v, want %v", err2, want)
	}
}

func TestBuilderWithNodeHintAndDeterministicSeed(t *testing.T) {
	b1 := NewBuilder(ModeDAG, 1, WithNodeHint(1000))
	if len(b1.visited.slots) < 2000 {
		t.Fatalf("WithNodeHint(1000) allocated only %d visited slots", len(b1.visited.slots))
	}
	b2 := NewBuilder(ModeDAG, 1)
	if b1.Seed == 0 || b2.Seed == 0 || b1.Seed == b2.Seed {
		t.Fatalf("expected distinct non-zero default seeds: %#x vs %#x", b1.Seed, b2.Seed)
	}
	b3 := NewBuilder(ModeDAG, 1, WithDeterministicSeed())
	if b3.Seed != DefaultSeed {
		t.Fatalf("WithDeterministicSeed got %#x, want %#x", b3.Seed, DefaultSeed)
	}
	var mf memFile
	cw, err := NewChunkedWriter(&mf, Header{SchemaHash: 1}, []SectionSpec{
		{Kind: SectionStrBlob, ElemAlign: 1, Count: 2, Len: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cw.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mf.buf, []byte("ok")) {
		t.Fatal("chunked payload missing")
	}
}
