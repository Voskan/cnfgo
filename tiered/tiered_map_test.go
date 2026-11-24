package tiered

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

func mustNew[K comparable, V any](t testing.TB, o *Options[V]) *TieredMap[K, V] {
	t.Helper()
	m, err := New[K, V](o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestBasic(t *testing.T) {
	m := mustNew[string, int](t, &Options[int]{MinFlushEntries: 8})
	for i := range 100 {
		m.Put(fmt.Sprint(i), i)
	}
	for i := 0; i < 100; i += 3 {
		m.Delete(fmt.Sprint(i))
	}
	check := func(stage string) {
		t.Helper()
		for i := range 100 {
			v, ok := m.Get(fmt.Sprint(i))
			if want := i%3 != 0; ok != want || (ok && v != i) {
				t.Fatalf("%s: Get(%d) = %d, %v", stage, i, v, ok)
			}
		}
		if n := m.Len(); n != 66 {
			t.Fatalf("%s: Len = %d, want 66", stage, n)
		}
	}
	check("before flush")
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	check("after flush")
	if st := m.Stats(); st.BaseLen != 66 || st.Flushing != 0 || st.Rotations == 0 || st.Compactions == 0 {
		t.Fatalf("stats %+v", st)
	}
	// A deletion of a compacted key must shadow the base until the next
	// compaction drops both.
	m.Delete("1")
	if m.Has("1") {
		t.Fatal("tombstone did not shadow the base")
	}
	m.Flush()
	if m.Has("1") || m.Base().Has("1") {
		t.Fatal("deleted key resurrected by compaction")
	}
}

func TestNewFromAndSnapshot(t *testing.T) {
	base, err := cnfrt.NewFrozenMap(map[int64]string{1: "a", 2: "b", 3: "c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewFrom(base, &Options[string]{MinFlushEntries: 1 << 20})
	defer m.Close()
	m.Put(4, "d")
	m.Delete(2)
	view := m.Snapshot()
	m.Put(5, "e")
	m.Delete(1)
	got := maps.Collect(view.All())
	want := map[int64]string{1: "a", 3: "c", 4: "d"}
	if !maps.Equal(got, want) || view.Len() != 3 {
		t.Fatalf("view = %v (Len %d), want %v", got, view.Len(), want)
	}
	if v, ok := view.Get(1); !ok || v != "a" {
		t.Fatal("view must not observe later deletions")
	}
	if _, ok := view.Get(5); ok {
		t.Fatal("view must not observe later writes")
	}
	all := map[int64]string{}
	m.Range(func(k int64, v string) bool { all[k] = v; return true })
	if !maps.Equal(all, map[int64]string{3: "c", 4: "d", 5: "e"}) {
		t.Fatalf("Range = %v", all)
	}
}

func TestBackpressureAfterClose(t *testing.T) {
	m := mustNew[int, int](t, &Options[int]{MinFlushEntries: 4, MaxFlushing: 1})
	m.Close()
	for i := range 1000 {
		m.Put(i, i)
	}
	if st := m.Stats(); st.Flushing > 1 {
		t.Fatalf("flushing grew to %d layers", st.Flushing)
	}
	for i := range 1000 {
		if v, ok := m.Get(i); !ok || v != i {
			t.Fatalf("Get(%d) = %d, %v", i, v, ok)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if m.Base().Len() != 1000 {
		t.Fatalf("base has %d entries", m.Base().Len())
	}
}

func TestCompactorStopsWhenUnreachable(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 20 {
		m, err := New[int, int](nil)
		if err != nil {
			t.Fatal(err)
		}
		m.Put(1, 1)
	}
	for i := 0; i < 100 && runtime.NumGoroutine() > before; i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines leaked", n-before)
	}
}

// TestStress runs 16 writers that each own a disjoint key range and keep a
// reference log, and 32 readers, with an aggressive rotation threshold.
// Writers check read-your-writes after every operation; readers check that
// values observed for a key never move backwards (no stale layer or
// resurrected deletion becomes visible after a newer write). Finally the map
// must match the reference exactly, before and after a full compaction.
func TestStress(t *testing.T) {
	const (
		writers       = 16
		readers       = 32
		keysPerWriter = 512
	)
	opsPerWriter := 32_000 // 512,000 operations in total
	if testing.Short() {
		opsPerWriter = 4_000
	}
	m := mustNew[uint64, uint64](t, &Options[uint64]{MinFlushEntries: 64, MaxFlushing: 4})
	key := func(w, k int) uint64 { return uint64(k)*writers + uint64(w) }
	// Values encode (sequence << 1) | 1 so that a reader can order them.
	refs := make([]map[uint64]uint64, writers)
	var stop atomic.Bool
	var violations atomic.Int64
	var wg, rg sync.WaitGroup
	for r := range readers {
		rg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(r), 99))
			last := map[uint64]uint64{}
			for !stop.Load() {
				k := key(rng.IntN(writers), rng.IntN(keysPerWriter))
				if v, ok := m.Get(k); ok {
					if v < last[k] {
						violations.Add(1)
					}
					last[k] = v
				}
			}
		})
	}
	for w := range writers {
		refs[w] = map[uint64]uint64{}
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			ref := refs[w]
			for op := range opsPerWriter {
				k := key(w, rng.IntN(keysPerWriter))
				if rng.IntN(4) == 0 {
					m.Delete(k)
					delete(ref, k)
					if m.Has(k) {
						violations.Add(1)
					}
					continue
				}
				v := uint64(op+1)<<1 | 1
				m.Put(k, v)
				ref[k] = v
				if got, ok := m.Get(k); !ok || got != v {
					violations.Add(1)
				}
			}
		})
	}
	wg.Wait()
	stop.Store(true)
	rg.Wait()
	if n := violations.Load(); n != 0 {
		t.Fatalf("%d consistency violations", n)
	}
	want := map[uint64]uint64{}
	for _, ref := range refs {
		maps.Copy(want, ref)
	}
	verify := func(stage string) {
		t.Helper()
		lost, zombies := 0, 0
		for w := range writers {
			for k := range keysPerWriter {
				kk := key(w, k)
				got, ok := m.Get(kk)
				wv, wok := want[kk]
				switch {
				case wok && (!ok || got != wv):
					lost++
				case !wok && ok:
					zombies++
				}
			}
		}
		got := map[uint64]uint64{}
		m.Range(func(k, v uint64) bool { got[k] = v; return true })
		if lost != 0 || zombies != 0 || !maps.Equal(got, want) || m.Len() != len(want) {
			t.Fatalf("%s: %d lost writes, %d zombie keys, Range has %d entries, want %d", stage, lost, zombies, len(got), len(want))
		}
	}
	verify("concurrent")
	st := m.Stats()
	t.Logf("%d operations, %d live keys, %d rotations, %d compactions", writers*opsPerWriter, len(want), st.Rotations, st.Compactions)
	if st.Rotations < 10 || st.Compactions == 0 {
		t.Fatalf("rotation threshold not exercised: %+v", st)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	verify("flushed")
	if m.Base().Len() != len(want) {
		t.Fatalf("base has %d entries after Flush, want %d", m.Base().Len(), len(want))
	}
}

func TestNaNKeys(t *testing.T) {
	type scored struct {
		ID    int
		Score float64
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fm := mustNew[float64, int](t, &Options[int]{MinFlushEntries: 4, MaxFlushing: 1})
		sm := mustNew[scored, int](t, &Options[int]{MinFlushEntries: 4, MaxFlushing: 1})
		if err := fm.TryPut(math.NaN(), 1); !errors.Is(err, cnfrt.ErrNaNKey) {
			t.Errorf("TryPut(NaN) = %v", err)
		}
		if err := sm.TryPut(scored{1, math.NaN()}, 1); !errors.Is(err, cnfrt.ErrNaNKey) {
			t.Errorf("TryPut({1, NaN}) = %v", err)
		}
		for i := range 2000 {
			fm.Put(math.NaN(), i)
			fm.Put(float64(i), i)
			fm.Delete(math.NaN())
			sm.Put(scored{i, math.NaN()}, i)
			sm.Put(scored{i, 1}, i)
		}
		for _, m := range []interface{ Flush() error }{fm, sm} {
			if err := m.Flush(); err != nil {
				t.Errorf("Flush: %v", err)
			}
		}
		if n := fm.Len(); n != 2000 {
			t.Errorf("float map Len = %d, want 2000", n)
		}
		if n := sm.Len(); n != 2000 {
			t.Errorf("struct map Len = %d, want 2000", n)
		}
		if _, ok := fm.Get(math.NaN()); ok {
			t.Error("NaN key found")
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("writers blocked by NaN keys")
	}
}

// TestSnapshotSeesObservedWrites checks that an entry a reader has observed
// is part of every later Snapshot and Flush.
func TestSnapshotSeesObservedWrites(t *testing.T) {
	m := mustNew[int, int](t, &Options[int]{MinFlushEntries: 1 << 20})
	const rounds = 20_000
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range rounds {
			m.Put(i, i)
		}
	})
	for i := range rounds {
		for !m.Has(i) {
			runtime.Gosched()
		}
		if i%2 == 0 {
			if _, ok := m.Snapshot().Get(i); !ok {
				t.Fatalf("Snapshot misses observed key %d", i)
			}
		} else if i%101 == 1 {
			if err := m.Flush(); err != nil {
				t.Fatal(err)
			}
			if !m.Base().Has(i) {
				t.Fatalf("Flush misses observed key %d", i)
			}
		}
	}
	wg.Wait()
}

func TestTieredMapCollisionReseed(t *testing.T) {
	const (
		n      = 2000
		groups = 512 // smallest power of two with groups*7 >= n
	)
	collidingKeys := func(seed uint64) []uint64 {
		keys := make([]uint64, 0, n)
		for k := uint64(0); len(keys) < n; k++ {
			if (cnfrt.HashUint64(k, seed)>>7)&(groups-1) == 0 {
				keys = append(keys, k)
			}
		}
		return keys
	}
	run := func(t *testing.T, tm *TieredMap[uint64, int]) {
		t.Helper()
		initSeed := tm.Base().Seed()
		keys := collidingKeys(initSeed)
		for i, k := range keys {
			tm.Put(k, i)
		}
		if err := tm.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := tm.Base().Seed(); got == initSeed {
			t.Fatalf("Base().Seed() stayed %#x after Flush; expected reseed", initSeed)
		}
		if err := tm.Base().Validate(); err != nil {
			t.Fatalf("Base().Validate(): %v", err)
		}
		for i, k := range keys {
			if v, ok := tm.Get(k); !ok || v != i {
				t.Fatalf("Get(%d) = %d, %v; want %d, true", k, v, ok, i)
			}
		}
	}

	t.Run("New", func(t *testing.T) {
		tm := mustNew[uint64, int](t, nil)
		run(t, tm)
	})
	t.Run("NewFrom", func(t *testing.T) {
		fm, err := cnfrt.EmptyFrozenMap[uint64, int](nil)
		if err != nil {
			t.Fatal(err)
		}
		tm := NewFrom(fm, nil)
		defer tm.Close()
		run(t, tm)
	})
}
