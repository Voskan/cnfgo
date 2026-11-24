package cnfrt

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestFlatTypesArePointerFree(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[NodeID](),
		reflect.TypeFor[Span32](),
		reflect.TypeFor[VariantTag](),
		reflect.TypeFor[FlatTime](),
		reflect.TypeFor[FlatAddr](),
		reflect.TypeFor[FlatPrefix](),
		reflect.TypeFor[FlatAddrPort](),
		reflect.TypeFor[visitedEntry](),
		reflect.TypeFor[internSlot](),
		reflect.TypeFor[[MaxInternSlots]internSlot](),
	} {
		if err := CheckNoPointers(typ); err != nil {
			t.Error(err)
		}
	}
}

func TestFlatTypeSizes(t *testing.T) {
	cases := []struct {
		typ  reflect.Type
		size uintptr
	}{
		{reflect.TypeFor[Span32](), 8},
		{reflect.TypeFor[VariantTag](), 8},
		{reflect.TypeFor[FlatTime](), 16},
		{reflect.TypeFor[FlatAddr](), 20},
		{reflect.TypeFor[FlatPrefix](), 20},
		{reflect.TypeFor[FlatAddrPort](), 24},
		{reflect.TypeFor[[MaxInternSlots]internSlot](), 16 << 20},
	}
	for _, c := range cases {
		if got := c.typ.Size(); got != c.size {
			t.Errorf("sizeof(%v) = %d, want %d", c.typ, got, c.size)
		}
	}
	if reflect.TypeFor[uintptr]().Size() == 8 {
		if sz := reflect.TypeFor[slabShard]().Size(); sz%128 != 0 {
			t.Errorf("sizeof(slabShard) = %d, want multiple of 128", sz)
		}
	}
}

func TestAssertNoPointersRejectsPointers(t *testing.T) {
	type inner struct{ S string }
	type outer struct {
		A int64
		B [2]inner
	}
	err := AssertNoPointers[outer]()
	if err == nil {
		t.Fatal("expected an error for a struct containing a string")
	}
	if err := AssertNoPointers[[0]*int](); err != nil {
		t.Errorf("zero-length array of pointers reported: %v", err)
	}
	for _, f := range []func() error{
		AssertNoPointers[*int], AssertNoPointers[[]byte], AssertNoPointers[map[int]int],
		AssertNoPointers[any], AssertNoPointers[func()], AssertNoPointers[chan int],
	} {
		if f() == nil {
			t.Error("pointer type accepted")
		}
	}
}

func TestSpan32(t *testing.T) {
	if !NilSpan.IsNil() || (Span32{}).IsNil() {
		t.Fatal("nil span sentinel")
	}
	if !NilSpan.InBounds(0) || (Span32{Off: NilSpanOff, Len: 1}).InBounds(math.MaxInt) {
		t.Fatal("nil span bounds")
	}
	if !(Span32{Off: 2, Len: 3}).InBounds(5) || (Span32{Off: 2, Len: 4}).InBounds(5) {
		t.Fatal("span bounds")
	}
	if (Span32{Off: 0xFFFFFFF0, Len: 0xFFFFFFF0}).InBounds(1 << 30) {
		t.Fatal("overflowing span accepted")
	}
}

func TestFailRecover(t *testing.T) {
	run := func() (err error) {
		defer func() { Recover(recover(), &err) }()
		Fail(ErrCapacityExceeded)
		return nil
	}
	if err := run(); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("got %v", err)
	}
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("foreign panic not propagated: %v", r)
		}
	}()
	func() {
		var err error
		defer func() { Recover(recover(), &err) }()
		panic("boom")
	}()
}

func TestUnsafeSeal(t *testing.T) {
	b := []byte("hello, world")
	s := UnsafeSealBytes(b)
	if s != "hello, world" || UnsafeSealBytes(nil) != "" {
		t.Fatal("seal")
	}
	if string(UnsafeStringBytes(s)) != s || UnsafeStringBytes("") != nil {
		t.Fatal("string bytes")
	}
	for n := 1; n < 200; n += 7 {
		src := make([]byte, n)
		for i := range src {
			src[i] = byte(i)
		}
		c := AlignedCopy(src)
		if Addr(&c[0])&63 != 0 || string(c) != string(src) || len(c) != n {
			t.Fatalf("aligned copy n=%d", n)
		}
	}
}

func TestAppendSliceLE(t *testing.T) {
	in := []Span32{{1, 2}, {0x01020304, 0xA0B0C0D0}}
	out := AppendSliceLE([]byte{9}, in)
	want := []byte{9, 1, 0, 0, 0, 2, 0, 0, 0, 4, 3, 2, 1, 0xD0, 0xC0, 0xB0, 0xA0}
	if string(out) != string(want) {
		t.Fatalf("got %x want %x", out, want)
	}
	if SliceSize(in) != 16 {
		t.Fatal("slice size")
	}
}

// TestLayoutMergedFieldsAligned checks that adjacent integer fields are only
// merged into 2-, 4- and 8-byte accesses that are naturally aligned, and
// that values of such types round-trip.
func TestLayoutMergedFieldsAligned(t *testing.T) {
	type flagged struct {
		Flag bool
		A, B uint8
	}
	type tagged struct {
		Tag  uint8
		_    [3]byte
		A, B uint32
	}
	type bytes4 struct{ A, B, C, D uint8 }
	type mixed struct {
		A    uint8
		B    uint16
		C    uint32
		D    uint8
		E, F uint16
	}
	check := func(name string, typ reflect.Type) {
		t.Helper()
		l, err := buildLayout(typ)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range l.fields {
			if s := uintptr(f.size); f.kind == lkInt && (s == 2 || s == 4 || s == 8) && (f.off%s != 0 || s > uintptr(typ.Align())) {
				t.Errorf("%s: %d-byte access at offset %d of a type aligned to %d", name, s, f.off, typ.Align())
			}
		}
	}
	check("flagged", reflect.TypeFor[flagged]())
	check("tagged", reflect.TypeFor[tagged]())
	check("bytes4", reflect.TypeFor[bytes4]())
	check("mixed", reflect.TypeFor[mixed]())
	roundTrip := func(v any, dec func() (any, error)) {
		t.Helper()
		got, err := dec()
		if err != nil || got != v {
			t.Fatalf("round trip of %+v: %+v, %v", v, got, err)
		}
	}
	f := flagged{true, 7, 9}
	fc, _ := DefaultCodec[flagged]()
	roundTrip(f, func() (any, error) { return fc.Decode(string(fc.Append(nil, f))) })
	g := tagged{Tag: 1, A: 0x01020304, B: 0x05060708}
	gc, _ := DefaultCodec[tagged]()
	roundTrip(g, func() (any, error) { return gc.Decode(string(gc.Append(nil, g))) })
	m := mixed{1, 2, 3, 4, 5, 6}
	mc, _ := DefaultCodec[mixed]()
	roundTrip(m, func() (any, error) { return mc.Decode(string(mc.Append(nil, m))) })
}
