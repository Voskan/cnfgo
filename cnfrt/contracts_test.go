package cnfrt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestChunkedWriterWriteAfterClose(t *testing.T) {
	h, secs := testHeader(), testSections()
	specs := make([]SectionSpec, len(secs))
	var payload []byte
	for i, s := range secs {
		specs[i] = SectionSpec{Kind: s.Kind, ElemAlign: s.ElemAlign, Tag: s.Tag, Count: s.Count, Len: uint64(len(s.Data))}
		payload = append(payload, s.Data...)
	}
	f := &memFile{}
	cw, err := NewChunkedWriter(f, h, specs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	image := bytes.Clone(f.buf)
	if n, err := cw.Write([]byte{1}); !errors.Is(err, ErrClosed) || n != 0 {
		t.Fatalf("Write after Close = %d, %v; want 0, ErrClosed", n, err)
	}
	if !bytes.Equal(f.buf, image) {
		t.Fatal("Write after Close modified the image")
	}
}

type edgeGraph struct {
	lens  []int
	edges map[[2]uint32][][2]uint32
}

func (g edgeGraph) TableLens() []int { return g.lens }

func (g edgeGraph) Edges(tbl int, id uint32, visit func(int, uint32)) {
	for _, e := range g.edges[[2]uint32{uint32(tbl), id}] {
		visit(int(e[0]), e[1])
	}
}

func TestCheckAcyclicErrors(t *testing.T) {
	dag := edgeGraph{lens: []int{2, 1}, edges: map[[2]uint32][][2]uint32{
		{0, 1}: {{0, 2}, {1, 1}},
		{0, 2}: {{1, 1}},
	}}
	if err := CheckAcyclic(dag); err != nil {
		t.Fatalf("CheckAcyclic(DAG) = %v", err)
	}
	cyclic := edgeGraph{lens: []int{2, 1}, edges: map[[2]uint32][][2]uint32{
		{0, 1}: {{1, 1}},
		{1, 1}: {{0, 2}},
		{0, 2}: {{0, 1}},
	}}
	err := CheckAcyclic(cyclic)
	if !errors.Is(err, ErrCycleDetected) || !errors.Is(err, ErrBoundsViolation) {
		t.Fatalf("CheckAcyclic(cycle) = %v, want ErrCycleDetected and ErrBoundsViolation", err)
	}
}

func TestFixedLocationLongNamesUncached(t *testing.T) {
	short := FixedLocation("UTC+3", 3*3600)
	if FixedLocation("UTC+3", 3*3600) != short {
		t.Fatal("short zone name is not cached")
	}
	long := strings.Repeat("z", maxFixedNameCache+1)
	a, b := FixedLocation(long, 60), FixedLocation(long, 60)
	if a == b {
		t.Fatal("zone name longer than the cache limit was cached")
	}
	if a.String() != long {
		t.Fatalf("location name = %q", a.String())
	}
	if m := fixedLocs.m.Load(); m != nil {
		for k := range *m {
			if len(k.name) > maxFixedNameCache {
				t.Fatalf("cache holds a %d-byte zone name", len(k.name))
			}
		}
	}
}

// TestFaultSafe covers error and panic pass-through; conversion of real
// memory faults is tested with memory-mapped files in package unsafemmap.
func TestFaultSafe(t *testing.T) {
	want := errors.New("plain")
	if err := FaultSafe(func() error { return want }); err != want {
		t.Fatalf("FaultSafe passed through %v", err)
	}
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v, want the original panic", r)
			}
		}()
		FaultSafe(func() error { panic("boom") })
	}()

	b, _ := NewFrozenBuilder[string, int64](1, nil)
	b.Add("k", 7)
	m, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	err = m.WithFaultSafe(func(m *FrozenMap[string, int64]) error {
		if v, ok := m.Get("k"); !ok || v != 7 {
			t.Errorf("Get = %d, %v", v, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type hashedA struct{ X int32 }

func (hashedA) SchemaHashCNF() uint64 { return 1 }

type hashedB struct{ X int32 }

func (hashedB) SchemaHashCNF() uint64 { return 2 }

type plainX struct{ X int32 }

func TestFrozenMapSchemaUsesSchemaHasher(t *testing.T) {
	a, b, p := FrozenMapSchema[string, hashedA](), FrozenMapSchema[string, hashedB](), FrozenMapSchema[string, plainX]()
	if a == b || a == p || b == p {
		t.Fatalf("schema hashes do not reflect SchemaHashCNF: %x %x %x", a, b, p)
	}
}

func TestFrozenBuilderReseedsEarly(t *testing.T) {
	b, err := NewFrozenBuilder[string, int64](0, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 2000
	for i := range n {
		b.Add(keyN(i), int64(i))
	}
	seed := b.seed
	// Pretend every key collides under the initial seed: the first attempt
	// exceeds the probe budget and the table is rebuilt under a new seed.
	clear(b.hashes)
	m, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if m.seed == seed {
		t.Fatal("Finish did not reseed a table that exceeded its probe budget")
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if v, ok := m.Get(keyN(i)); !ok || v != int64(i) {
			t.Fatalf("Get(%s) = %d, %v", keyN(i), v, ok)
		}
	}
}

// TestFrozenMapValidateComparisonBudget builds a table whose keys were
// chosen to collide under a known fixed seed: every key has the same
// control byte and home group, so each lookup compares it with all keys
// before it. The probe count stays within budget, but the bytes compared
// grow quadratically and Validate must reject the table.
func TestFrozenMapValidateComparisonBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("searches for colliding keys")
	}
	const (
		seed   = 0x9e3779b97f4a7c15
		n      = 120
		keyLen = 1024
		groups = 32 // for n entries at a 7/8 load factor
	)
	buf := bytes.Repeat([]byte{'k'}, keyLen)
	var keys []string
	var want uint64
	for i := uint64(0); len(keys) < n; i++ {
		binary.LittleEndian.PutUint64(buf, i)
		h := HashString(string(buf), seed)
		sig := h&0x7f | (h>>7)&(groups-1)<<7
		if len(keys) == 0 {
			want = sig
		}
		if sig == want {
			keys = append(keys, string(buf))
		}
	}
	build := func(o *MapOptions[uint8]) *FrozenMap[string, uint8] {
		b, err := NewFrozenBuilder[string, uint8](n, o)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			b.Add(k, 1)
		}
		m, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := build(&MapOptions[uint8]{Seed: seed})
	if m.numGroups != groups {
		t.Fatalf("table has %d groups, want %d", m.numGroups, groups)
	}
	if err := m.Validate(); !errors.Is(err, ErrBoundsViolation) || !strings.Contains(err.Error(), "comparison budget") {
		t.Fatalf("Validate(colliding keys) = %v, want comparison budget violation", err)
	}
	// Under a random seed the same keys spread out and validate.
	if err := build(nil).Validate(); err != nil {
		t.Fatalf("Validate(random seed) = %v", err)
	}
}
