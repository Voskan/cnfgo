package cnfrt

import (
	"errors"
	"iter"
	"math"
	mathrand "math/rand"
	"math/rand/v2"
	"runtime"
	"runtime/metrics"
	"strconv"
	"testing"
)

// scanHeapBytes returns the size of the scannable heap after a full
// collection.
func scanHeapBytes() uint64 {
	runtime.GC()
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/scan/heap:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s[0].Value.Uint64()
}

func keyN(i int) string { return "key/" + strconv.Itoa(i) + "/suffix" }

func TestFrozenMapMillionKeys(t *testing.T) {
	const n = 1_000_000
	before := scanHeapBytes()
	seq := func(yield func(string, int64) bool) {
		for i := range n {
			if !yield(keyN(i), int64(i)*3) {
				return
			}
		}
	}
	m, err := BuildFrozenMap(iter.Seq2[string, int64](seq), nil)
	if err != nil {
		t.Fatal(err)
	}
	after := scanHeapBytes()
	delta := int64(after) - int64(before)
	t.Logf("scannable heap delta with %d-entry FrozenMap alive: %d bytes", n, delta)
	if delta > 512<<10 {
		t.Fatalf("FrozenMap adds %d bytes of scannable heap, want < 512 KiB", delta)
	}
	if m.Len() != n {
		t.Fatalf("Len = %d", m.Len())
	}
	for i := 0; i < n; i += 7 {
		v, ok := m.Get(keyN(i))
		if !ok || v != int64(i)*3 {
			t.Fatalf("Get(%d) = %d, %v", i, v, ok)
		}
	}
	for i := n; i < n+1000; i++ {
		if m.Has(keyN(i)) {
			t.Fatalf("phantom key %d", i)
		}
	}
	if err := AssertNoPointers[FrozenSlot](); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(m)
}

func TestFrozenMapGetAllocs(t *testing.T) {
	m, err := NewFrozenMap(map[string]int64{"a": 1, "bb": 2, "ccc": 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoAllocs(t, "string-key Get", func() { _, _ = m.Get("bb") })
	mi, _ := NewFrozenMap(map[uint64]float64{1: 1.5, 2: 2.5}, nil)
	assertNoAllocs(t, "uint64-key Get", func() { _, _ = mi.Get(2) })
	ms, _ := NewFrozenMap(map[string]string{"k": "value"}, nil)
	assertNoAllocs(t, "string value Get", func() { _, _ = ms.Get("k") })
}

type compositeKey struct {
	A int8
	B float64
	C string
	D [2]float32
	E bool
}

func TestFrozenMapFloatKeys(t *testing.T) {
	negZero := math.Copysign(0, -1)
	m, err := NewFrozenMap(map[float64]string{negZero: "zero", 1.5: "x", math.Inf(1): "inf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := m.Get(0); !ok || v != "zero" {
		t.Fatal("+0.0 does not find the -0.0 key")
	}
	if v, ok := m.Get(negZero); !ok || v != "zero" {
		t.Fatal("-0.0")
	}
	if _, ok := m.Get(math.NaN()); ok {
		t.Fatal("NaN found")
	}
	assertNoAllocs(t, "NaN lookup", func() { m.Get(math.NaN()) })
	if _, err := NewFrozenMap(map[float64]int{math.NaN(): 1}, nil); !errors.Is(err, ErrNaNKey) {
		t.Fatalf("NaN key accepted: %v", err)
	}
	// Float values keep their exact bits, including -0.0 and NaN payloads.
	payload := math.Float64frombits(0x7ff8000000000123)
	mv, _ := NewFrozenMap(map[string]float64{"nz": negZero, "nan": payload}, nil)
	if v, _ := mv.Get("nz"); math.Float64bits(v) != math.Float64bits(negZero) {
		t.Fatal("-0.0 value canonicalized")
	}
	if v, _ := mv.Get("nan"); math.Float64bits(v) != 0x7ff8000000000123 {
		t.Fatal("NaN payload lost")
	}
}

func TestFrozenMapCompositeKeys(t *testing.T) {
	k1 := compositeKey{A: -3, B: math.Copysign(0, -1), C: "alpha", D: [2]float32{1, float32(math.Copysign(0, -1))}, E: true}
	k2 := compositeKey{A: 4, B: 2.5, C: "", D: [2]float32{3, 4}}
	m, err := NewFrozenMap(map[compositeKey]int{k1: 1, k2: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := k1
	q.B, q.D[1] = 0, 0
	if v, ok := m.Get(q); !ok || v != 1 {
		t.Fatal("canonical composite key not found")
	}
	if v, ok := m.Get(k2); !ok || v != 2 {
		t.Fatal("k2")
	}
	bad := k2
	bad.D[0] = float32(math.NaN())
	if _, ok := m.Get(bad); ok {
		t.Fatal("NaN composite found")
	}
	if _, err := NewFrozenMap(map[compositeKey]int{bad: 1}, nil); !errors.Is(err, ErrNaNKey) {
		t.Fatal("NaN composite accepted")
	}
	got := map[compositeKey]int{}
	for k, v := range m.All() {
		got[k] = v
	}
	if len(got) != 2 || got[k2] != 2 || got[q] != 1 {
		t.Fatalf("All = %v", got)
	}
	if _, err := NewFrozenMap(map[*int]int{}, nil); err == nil {
		t.Fatal("pointer keys accepted")
	}
}

func TestFrozenMapDuplicatesLastWins(t *testing.T) {
	seq := func(yield func(string, int) bool) {
		for i := range 100 {
			if !yield(strconv.Itoa(i%10), i) {
				return
			}
		}
	}
	m, err := BuildFrozenMap(iter.Seq2[string, int](seq), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Len() != 10 {
		t.Fatalf("Len = %d", m.Len())
	}
	if v, _ := m.Get("3"); v != 93 {
		t.Fatalf("Get(3) = %d", v)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenMapCorruptCtrlTerminates(t *testing.T) {
	src := map[string]int{}
	for i := range 5000 {
		src[keyN(i)] = i
	}
	m, err := NewFrozenMap(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	sec := m.Sections()
	ctrl := append([]byte(nil), sec.Ctrl...)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range ctrl {
		ctrl[i] = byte(r.IntN(0x80)) // every slot "full", no empty slot anywhere
	}
	sec.Ctrl = ctrl
	bad, err := FrozenMapFromSections[string, int](sec, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		bad.Get(keyN(1_000_000 + i)) // must terminate
	}
	if _, err := FrozenMapFromSections[string, int](sec, nil, true); err == nil {
		t.Fatal("validation accepted a corrupt control array")
	}
	slots := append([]FrozenSlot(nil), m.Sections().Slots...)
	for i := range slots {
		slots[i] = FrozenSlot{KeyOff: math.MaxUint32, KeyLen: 9, ValOff: math.MaxUint32 - 1, ValLen: 99}
	}
	sec2 := m.Sections()
	sec2.Slots = slots
	bad2, err := FrozenMapFromSections[string, int](sec2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		bad2.Get(keyN(i))
	}
	if _, err := FrozenMapFromSections[string, int](sec2, nil, true); err == nil {
		t.Fatal("validation accepted corrupt slots")
	}
	geo := m.Sections()
	geo.NumGroups = 3
	if _, err := FrozenMapFromSections[string, int](geo, nil, false); err == nil {
		t.Fatal("bad geometry accepted")
	}
}

func TestFrozenMapSectionsRoundTrip(t *testing.T) {
	src := map[int32]string{}
	for i := range 3000 {
		src[int32(i*7-1000)] = "v" + strconv.Itoa(i)
	}
	m, err := NewFrozenMap(src, &MapOptions[string]{Seed: 12345})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := FrozenMapFromSections[int32, string](m.Sections(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Seed() != 12345 {
		t.Fatal("seed")
	}
	for k, v := range src {
		if got, ok := m2.Get(k); !ok || got != v {
			t.Fatalf("Get(%d)", k)
		}
	}
	n := 0
	for k, v := range m2.RawEntries() {
		dk, err := m2.DecodeKey(k)
		if err != nil || src[dk] == "" || len(v) == 0 {
			t.Fatal("raw entries")
		}
		n++
	}
	if n != len(src) {
		t.Fatal("raw entry count")
	}
	e, err := EmptyFrozenMap[string, int](nil)
	if err != nil || e.Len() != 0 || e.Has("x") {
		t.Fatal("empty map")
	}
	var nilMap *FrozenMap[string, int]
	if nilMap.Len() != 0 || nilMap.Has("x") {
		t.Fatal("nil map")
	}
}

func BenchmarkFrozenMapGetString(b *testing.B) {
	src := map[string]int64{}
	for i := range 100_000 {
		src[keyN(i)] = int64(i)
	}
	m, _ := NewFrozenMap(src, nil)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = keyN(i * 97)
	}
	i := 0
	for b.Loop() {
		m.Get(keys[i&1023])
		i++
	}
}

func BenchmarkGoMapGetString(b *testing.B) {
	src := map[string]int64{}
	for i := range 100_000 {
		src[keyN(i)] = int64(i)
	}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = keyN(i * 97)
	}
	i := 0
	for b.Loop() {
		_ = src[keys[i&1023]]
		i++
	}
}

// TestFrozenMapValidateRejectsInconsistentTables builds tables whose slots
// are individually well formed but that a lookup cannot use consistently:
// no empty slot, a duplicated key, a key outside its probe sequence and a
// non-canonical key encoding.
func TestFrozenMapValidateRejectsInconsistentTables(t *testing.T) {
	src := map[string]int{}
	for i := range 3000 {
		src[keyN(i)] = i
	}
	m, err := NewFrozenMap(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	clone := func() FrozenSections {
		sec := m.Sections()
		sec.Ctrl = append([]byte(nil), sec.Ctrl...)
		sec.Slots = append([]FrozenSlot(nil), sec.Slots...)
		return sec
	}
	firstFull := func(sec FrozenSections) int {
		for i, c := range sec.Ctrl {
			if c != ctrlEmpty {
				return i
			}
		}
		t.Fatal("no full slot")
		return -1
	}
	if _, err := FrozenMapFromSections[string, int](clone(), nil, true); err != nil {
		t.Fatalf("valid table rejected: %v", err)
	}

	full := clone()
	i := firstFull(full)
	for j, c := range full.Ctrl {
		if c == ctrlEmpty {
			full.Ctrl[j], full.Slots[j] = full.Ctrl[i], full.Slots[i]
		}
	}
	full.Len = uint32(len(full.Ctrl))
	if _, err := FrozenMapFromSections[string, int](full, nil, false); err == nil {
		t.Fatal("table without an empty slot accepted")
	}

	emptyIn := func(sec FrozenSections, g int) int {
		for k := g * groupSlots; k < (g+1)*groupSlots; k++ {
			if sec.Ctrl[k] == ctrlEmpty {
				return k
			}
		}
		return -1
	}
	dup := clone()
	i, j := -1, -1
	for k, c := range dup.Ctrl {
		if c != ctrlEmpty {
			if e := emptyIn(dup, k/groupSlots); e >= 0 {
				i, j = k, e
				break
			}
		}
	}
	if i < 0 {
		t.Fatal("every group with a key is full")
	}
	dup.Ctrl[j], dup.Slots[j] = dup.Ctrl[i], dup.Slots[i]
	dup.Len++
	if _, err := FrozenMapFromSections[string, int](dup, nil, true); err == nil {
		t.Fatal("duplicate key accepted")
	}

	// Moving a key out of its home group leaves an empty slot there, at
	// which every lookup of the key stops.
	moved := clone()
	i = firstFull(moved)
	j = -1
	for d := 1; d < int(moved.NumGroups) && j < 0; d++ {
		j = emptyIn(moved, (i/groupSlots+d)%int(moved.NumGroups))
	}
	if j < 0 {
		t.Fatal("no other group has an empty slot")
	}
	moved.Ctrl[j], moved.Slots[j] = moved.Ctrl[i], moved.Slots[i]
	moved.Ctrl[i] = ctrlEmpty
	bad, err := FrozenMapFromSections[string, int](moved, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := bad.DecodeKey(moved.Data[moved.Slots[j].KeyOff : moved.Slots[j].KeyOff+moved.Slots[j].KeyLen])
	if bad.Has(k) {
		t.Fatal("moved key is still reachable")
	}
	if _, err := FrozenMapFromSections[string, int](moved, nil, true); err == nil {
		t.Fatal("key outside its probe sequence accepted")
	}

	fm, err := NewFrozenMap(map[float64]int{0: 1, 1: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fsec := fm.Sections()
	data := []byte(fsec.Data)
	for i, c := range fsec.Ctrl {
		if c == ctrlEmpty {
			continue
		}
		s := fsec.Slots[i]
		if string(data[s.KeyOff:s.KeyOff+s.KeyLen]) == string(make([]byte, 8)) {
			data[s.KeyOff+7] = 0x80 // -0.0: iterated, never found
		}
	}
	fsec.Data = string(data)
	if _, err := FrozenMapFromSections[float64, int](fsec, nil, true); err == nil {
		t.Fatal("non-canonical key accepted")
	}
}

type schemaNode struct {
	ID   int64
	Next *schemaNode
}

func TestFrozenMapSchemaIsStructural(t *testing.T) {
	type local struct{ A, B int32 }
	type reordered struct{ B, A int32 }
	type widened struct{ A, B int64 }
	hashes := map[uint64]string{}
	add := func(name string, h uint64) {
		t.Helper()
		if prev, dup := hashes[h]; dup {
			t.Fatalf("%s and %s have the same schema hash", prev, name)
		}
		hashes[h] = name
	}
	add("string/int64", FrozenMapSchema[string, int64]())
	add("string/int32", FrozenMapSchema[string, int32]())
	add("string/uint32", FrozenMapSchema[string, uint32]())
	add("int64/string", FrozenMapSchema[int64, string]())
	add("string/local", FrozenMapSchema[string, local]())
	add("string/reordered", FrozenMapSchema[string, reordered]())
	add("string/widened", FrozenMapSchema[string, widened]())
	add("string/math/rand.Zipf", FrozenMapSchema[string, *mathrand.Zipf]())
	add("string/math/rand/v2.Zipf", FrozenMapSchema[string, *rand.Zipf]())
	add("string/recursive", FrozenMapSchema[string, schemaNode]())
	if FrozenMapSchema[string, schemaNode]() != FrozenMapSchema[string, schemaNode]() {
		t.Fatal("schema hash is not deterministic")
	}
}
