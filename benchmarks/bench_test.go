package benchmarks

import (
	"math/rand/v2"
	"runtime"
	"testing"

	"github.com/Voskan/cnfgo/cnfrt"
)

func benchN() int {
	if testing.Short() {
		return 10_000
	}
	return 100_000
}

func TestStoresAgree(t *testing.T) {
	const n = 3000
	c := MakeCatalog(n)
	snap, err := FreezeCatalog(c)
	if err != nil {
		t.Fatal(err)
	}
	if snap.LenSeller() != NumSellers {
		t.Fatalf("sellers stored %d times, want %d", snap.LenSeller(), NumSellers)
	}
	cm := map[uint64]CompactItem{}
	cmDict := map[uint64]CompactItem{}
	dict := cnfrt.NewDict(cnfrt.DefaultDictMaxBytes)
	defer dict.Close()
	sm, err := cnfrt.NewSlabMap[uint64, CompactItem](nil)
	if err != nil {
		t.Fatal(err)
	}
	smDict, err := cnfrt.NewSlabMap[uint64, CompactItem](nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, it := range c.Items {
		p, err := PackItem(it)
		if err != nil {
			t.Fatal(err)
		}
		cm[id] = p
		if err := sm.Put(id, p); err != nil {
			t.Fatal(err)
		}
		pd, err := PackItem(it, cnfrt.WithDict(dict))
		if err != nil {
			t.Fatal(err)
		}
		cmDict[id] = pd
		if err := smDict.Put(id, pd); err != nil {
			t.Fatal(err)
		}
	}
	stores := []Store{NativeStore{c}, CompactStore{cm}, CompactStore{cmDict}, SlabStore{sm}, SlabBorrowStore{sm}, SlabStore{smDict}, SlabBorrowStore{smDict}, SnapshotStore{snap}}
	for id := uint64(0); id <= n+1; id++ {
		p0, l0, ok0 := stores[0].Read(id)
		for i, s := range stores[1:] {
			if p, l, ok := s.Read(id); p != p0 || l != l0 || ok != ok0 {
				t.Fatalf("store %d: Read(%d) = %d, %d, %v; want %d, %d, %v", i+1, id, p, l, ok, p0, l0, ok0)
			}
		}
	}
}

func TestScenarios(t *testing.T) {
	for _, s := range Scenarios {
		r, err := Run(s, 2000, 64<<20, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		t.Logf("%+v", r)
		if r.Heap == 0 && s != "snapshot-mmap" {
			t.Errorf("%s: no heap attributed to the store", s)
		}
		if r.ChurnGCs == 0 {
			t.Errorf("%s: the churn workload triggered no collection", s)
		}
	}
}

func TestColdStart(t *testing.T) {
	dir := t.TempDir()
	if err := WriteColdStartFiles(2000, dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range ColdStarts {
		d, size, err := ColdStart(f, dir, 1000)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Logf("%s: %d bytes, %v", f, size, d)
	}
}

func BenchmarkRead(b *testing.B) {
	n := benchN()
	c := MakeCatalog(n)
	snap, err := FreezeCatalog(c)
	if err != nil {
		b.Fatal(err)
	}
	cm := make(map[uint64]CompactItem, n)
	sm, _ := cnfrt.NewSlabMap[uint64, CompactItem](nil)
	for id, it := range c.Items {
		p, err := PackItem(it)
		if err != nil {
			b.Fatal(err)
		}
		cm[id] = p
		sm.Put(id, p)
	}
	for _, bc := range []struct {
		name string
		s    Store
	}{
		{"Native", NativeStore{c}},
		{"CompactMap", CompactStore{cm}},
		{"SlabMap", SlabStore{sm}},
		{"SlabMapBorrow", SlabBorrowStore{sm}},
		{"Snapshot", SnapshotStore{snap}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			rng := rand.New(rand.NewPCG(1, 2))
			var sum int64
			for b.Loop() {
				p, l, _ := bc.s.Read(rng.Uint64N(uint64(n)) + 1)
				sum += p + int64(l)
			}
			runtime.KeepAlive(sum)
		})
	}
}

func BenchmarkFreeze(b *testing.B) {
	c := MakeCatalog(benchN())
	b.ReportAllocs()
	for b.Loop() {
		if _, err := FreezeCatalog(c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPack(b *testing.B) {
	it := MakeItem(42, &Seller{ID: 1, Name: "Seller", Country: "DE"})
	b.Run("Standalone", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := PackItem(it); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("WithDict", func(b *testing.B) {
		d := cnfrt.NewDict(cnfrt.DefaultDictMaxBytes)
		defer d.Close()
		_, _ = PackItem(it, cnfrt.WithDict(d))
		_, _ = PackItem(it, cnfrt.WithDict(d))
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if _, err := PackItem(it, cnfrt.WithDict(d)); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkFullGC(b *testing.B) {
	n := benchN()
	run := func(b *testing.B, keep any) {
		runtime.GC()
		for b.Loop() {
			runtime.GC()
		}
		runtime.KeepAlive(keep)
	}
	b.Run("Native", func(b *testing.B) { run(b, MakeCatalog(n)) })
	b.Run("Snapshot", func(b *testing.B) {
		s, err := FreezeCatalog(MakeCatalog(n))
		if err != nil {
			b.Fatal(err)
		}
		run(b, s)
	})
}

func TestMeasureFreeze(t *testing.T) {
	f, err := MeasureFreeze(20_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", f)
	if f.Peak == 0 || f.Peak < uint64(f.Tables) || f.Allocated < uint64(f.Tables) {
		t.Fatalf("implausible freeze cost %+v", f)
	}
}

// TestPackCost bounds the garbage produced by packing a single item: the
// freeze state of a one-object graph must not dominate the packed value.
func TestPackCost(t *testing.T) {
	it := MakeItem(42, &Seller{ID: 1, Name: "Seller", Country: "DE"})
	c, err := PackItem(it)
	if err != nil {
		t.Fatal(err)
	}
	r := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			PackItem(it)
		}
	})
	t.Logf("PackItem: %d B/op, %d allocs/op for a %d-byte value", r.AllocedBytesPerOp(), r.AllocsPerOp(), len(c.Raw()))
	if limit := int64(8 * len(c.Raw())); r.AllocedBytesPerOp() > limit {
		t.Fatalf("PackItem allocates %d bytes per call, want at most %d", r.AllocedBytesPerOp(), limit)
	}
	// Pack reuses pooled freeze state, so the zero-copy build allocates the
	// returned value only. The portable build encodes flat tables with
	// encoding/binary, which allocates per call, and the race detector makes
	// sync.Pool drop items at random.
	if cnfrt.NativeLittleEndian && !raceEnabled && r.AllocsPerOp() > 2 {
		t.Fatalf("PackItem makes %d allocations per call, want at most 2", r.AllocsPerOp())
	}
}

// TestFreezeAllocsIndependentOfMapCount checks that the number of
// allocations made by Freeze does not grow with the number of maps in the
// source graph (every Item has an Attrs map).
func TestFreezeAllocsIndependentOfMapCount(t *testing.T) {
	allocs := func(n int) float64 {
		c := MakeCatalog(n)
		return testing.AllocsPerRun(2, func() {
			if _, err := FreezeCatalog(c); err != nil {
				t.Fatal(err)
			}
		})
	}
	small, large := allocs(1000), allocs(20_000)
	t.Logf("FreezeCatalog: %.0f allocations for 1000 items, %.0f for 20000", small, large)
	if large > small+16 {
		t.Fatalf("allocations grow with the number of maps: %.0f for 1000 items, %.0f for 20000", small, large)
	}
}
