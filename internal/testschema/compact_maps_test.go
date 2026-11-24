package testschema

import (
	"strconv"
	"testing"
	"unsafe"

	"github.com/Voskan/cnfgo/cnfrt"
	"github.com/Voskan/cnfgo/unsafemmap"
)

func compactEndpoints(t *testing.T, n int) map[string]CompactEndpoint {
	t.Helper()
	out := make(map[string]CompactEndpoint, n)
	for i := range n {
		e := &Endpoint{Name: "endpoint-" + strconv.Itoa(i), Port: 443, Tags: []string{"edge", "tls"}, Fee: NewMoney(int64(i), "EUR")}
		c, err := PackEndpoint(e)
		if err != nil {
			t.Fatal(err)
		}
		// Keys of varying length would misalign values stored right after
		// them.
		out["k"+strconv.Itoa(i*7919)] = c
	}
	return out
}

// TestCompactValuesAlignedInFrozenMap checks that Compact values stored in a
// FrozenMap start at 8-byte aligned addresses, both in a built map and in a
// map loaded zero-copy from its image, so that View casts the embedded
// tables in place instead of copying them.
func TestCompactValuesAlignedInFrozenMap(t *testing.T) {
	src := compactEndpoints(t, 200)
	built, err := cnfrt.NewFrozenMap(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	img, err := built.MarshalCNF()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := unsafemmap.FrozenMapFromImage[string, CompactEndpoint](cnfrt.AlignedCopy(img), cnfrt.VerifyChecksum, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*cnfrt.FrozenMap[string, CompactEndpoint]{"built": built, "loaded": loaded} {
		for k, want := range src {
			c, ok := m.Get(k)
			if !ok || !c.Equal(want) {
				t.Fatalf("%s: Get(%q)", name, k)
			}
			if p := uintptr(unsafe.Pointer(unsafe.StringData(c.Raw()))); p%8 != 0 {
				t.Fatalf("%s: value of %q at %#x is not 8-byte aligned", name, k, p)
			}
		}
	}
	c, _ := built.Get("k0")
	ref := src["k0"]
	if !instrumented() {
		got := testing.AllocsPerRun(50, func() { _ = c.View().Name() })
		want := testing.AllocsPerRun(50, func() { _ = ref.View().Name() })
		if got > want {
			t.Fatalf("View of a stored value makes %v allocations, a freshly packed one %v", got, want)
		}
	}
}

// TestCompactValuesInSlabMap checks that SlabMap decodes Compact values
// through their concrete codec: one copy per Get and no interface boxing.
func TestCompactValuesInSlabMap(t *testing.T) {
	m, err := cnfrt.NewSlabMap[string, CompactEndpoint](nil)
	if err != nil {
		t.Fatal(err)
	}
	src := compactEndpoints(t, 500)
	for k, v := range src {
		if err := m.Put(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for k, want := range src {
		got, ok := m.Get(k)
		if !ok || !got.Equal(want) || got.Name() != want.Name() {
			t.Fatalf("Get(%q)", k)
		}
	}
	if !instrumented() {
		if n := testing.AllocsPerRun(100, func() { m.Get("k0") }); n > 1 {
			t.Fatalf("SlabMap.Get of a Compact value: %v allocs/op, want at most 1", n)
		}
	}
	// Keys of lengths 1..8 must still yield 8-byte aligned slices in Borrow
	// so BorrowCompactEndpointBytes + View casts tables in place without copying.
	for kl := 1; kl <= 8; kl++ {
		k := "abcdefgh"[:kl]
		e := &Endpoint{Name: "aligned-" + strconv.Itoa(kl), Port: uint16(8000 + kl), Tags: []string{"a", "b"}, Fee: NewMoney(int64(kl), "USD")}
		c, err := PackEndpoint(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Put(k, c); err != nil {
			t.Fatal(err)
		}
		ok := m.Borrow(k, func(b []byte) {
			if p := uintptr(unsafe.Pointer(&b[0])); p%8 != 0 {
				t.Errorf("SlabMap.Borrow(%q) ptr %#x is not 8-byte aligned", k, p)
			}
			bc, err := BorrowCompactEndpointBytes(b)
			if err != nil {
				t.Errorf("BorrowCompactEndpointBytes(%q): %v", k, err)
				return
			}
			if got := bc.View().Name(); got != e.Name {
				t.Errorf("View().Name() = %q, want %q", got, e.Name)
			}
		})
		if !ok {
			t.Fatalf("SlabMap.Borrow(%q) missed", k)
		}
		if !instrumented() {
			want := testing.AllocsPerRun(50, func() { _ = c.View().Name() })
			got := testing.AllocsPerRun(50, func() {
				m.Borrow(k, func(b []byte) {
					bc, _ := BorrowCompactEndpointBytes(b)
					_ = bc.View().Name()
				})
			})
			if got > want {
				t.Fatalf("SlabMap.Borrow(%q) + View makes %v allocations, freshly packed %v", k, got, want)
			}
		}
	}
}

// checkptrProbe converts the address of a local array. Strict checkptr
// instrumentation passes the pointer to the runtime, which makes the array
// escape, so the probe allocates exactly when that instrumentation is on.
//
//go:noinline
func checkptrProbe() uint64 {
	var x [16]byte
	return *(*uint64)(unsafe.Pointer(&x[0]))
}

// instrumented reports whether the test binary is built with the race
// detector or checkptr instrumentation, under which allocation counts
// differ.
func instrumented() bool {
	if raceEnabled {
		return true
	}
	return testing.AllocsPerRun(10, func() { checkptrProbe() }) != 0
}
