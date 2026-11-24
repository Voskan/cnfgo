package tiered

import (
	"fmt"
	"hash/maphash"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

// store is the operation set shared by the compared maps.
type store[K comparable, V any] interface {
	Get(k K) (V, bool)
	Put(k K, v V)
	Delete(k K)
}

type syncMap[K comparable, V any] struct{ m sync.Map }

func (s *syncMap[K, V]) Get(k K) (V, bool) {
	v, ok := s.m.Load(k)
	if !ok {
		var zero V
		return zero, false
	}
	return v.(V), true
}
func (s *syncMap[K, V]) Put(k K, v V) { s.m.Store(k, v) }
func (s *syncMap[K, V]) Delete(k K)   { s.m.Delete(k) }

type rwShard[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
	_  [88]byte // pad to 128 bytes
}

type shardedMap[K comparable, V any] struct {
	seed   maphash.Seed
	shards [numShards]rwShard[K, V]
}

func newShardedMap[K comparable, V any]() *shardedMap[K, V] {
	s := &shardedMap[K, V]{seed: maphash.MakeSeed()}
	for i := range s.shards {
		s.shards[i].m = map[K]V{}
	}
	return s
}

func (s *shardedMap[K, V]) shard(k K) *rwShard[K, V] {
	return &s.shards[maphash.Comparable(s.seed, k)&(numShards-1)]
}

func (s *shardedMap[K, V]) Get(k K) (V, bool) {
	sh := s.shard(k)
	sh.mu.RLock()
	v, ok := sh.m[k]
	sh.mu.RUnlock()
	return v, ok
}

func (s *shardedMap[K, V]) Put(k K, v V) {
	sh := s.shard(k)
	sh.mu.Lock()
	sh.m[k] = v
	sh.mu.Unlock()
}

func (s *shardedMap[K, V]) Delete(k K) {
	sh := s.shard(k)
	sh.mu.Lock()
	delete(sh.m, k)
	sh.mu.Unlock()
}

func benchKeys(b *testing.B) int {
	if testing.Short() {
		return 100_000
	}
	return 1_000_000
}

// runMixedU64 performs 95% Get and 5% Put/Delete operations from parallel
// goroutines and reports the 99th percentile latency of sampled operations.
// No background allocator runs during the loop, so -benchmem reports only
// allocations made by the map under test.
func runMixedU64(b *testing.B, s store[uint64, uint64], n int) {
	var mu sync.Mutex
	var samples []time.Duration
	var seq atomic.Uint64
	runtime.GC()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewPCG(seq.Add(1), 1))
		local := make([]time.Duration, 0, 4096)
		for i := 0; pb.Next(); i++ {
			k := rng.Uint64N(uint64(n))
			sample := i&63 == 0
			var t0 time.Time
			if sample {
				t0 = time.Now()
			}
			switch r := rng.IntN(100); {
			case r < 95:
				s.Get(k)
			case r < 98:
				s.Put(k, k+1)
			default:
				s.Delete(k)
			}
			if sample {
				local = append(local, time.Since(t0))
			}
		}
		mu.Lock()
		samples = append(samples, local...)
		mu.Unlock()
	})
	b.StopTimer()
	if len(samples) > 0 {
		slices.Sort(samples)
		b.ReportMetric(float64(samples[len(samples)*99/100].Nanoseconds()), "p99-ns")
	}
}

func BenchmarkMixed(b *testing.B) {
	n := benchKeys(b)
	b.Run("TieredMap", func(b *testing.B) {
		fb, _ := cnfrt.NewFrozenBuilder[uint64, uint64](n, nil)
		for i := range n {
			fb.Add(uint64(i), uint64(i))
		}
		base, err := fb.Finish()
		if err != nil {
			b.Fatal(err)
		}
		m := NewFrom(base, nil)
		defer m.Close()
		runMixedU64(b, m, n)
	})
	b.Run("SyncMap", func(b *testing.B) {
		s := &syncMap[uint64, uint64]{}
		for i := range n {
			s.Put(uint64(i), uint64(i))
		}
		runMixedU64(b, s, n)
	})
	b.Run("ShardedRWMutexMap", func(b *testing.B) {
		s := newShardedMap[uint64, uint64]()
		for i := range n {
			s.Put(uint64(i), uint64(i))
		}
		runMixedU64(b, s, n)
	})
}

// BenchmarkParallelReadHot compares concurrent read throughput on a hot
// working set (1024 keys out of N) across all CPUs.
func BenchmarkParallelReadHot(b *testing.B) {
	n := benchKeys(b)
	const hot = 1024
	run := func(b *testing.B, s store[uint64, uint64]) {
		var seq atomic.Uint64
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			rng := rand.New(rand.NewPCG(seq.Add(1), 7))
			for pb.Next() {
				s.Get(rng.Uint64N(hot))
			}
		})
	}
	b.Run("TieredMap", func(b *testing.B) {
		fb, _ := cnfrt.NewFrozenBuilder[uint64, uint64](n, nil)
		for i := range n {
			fb.Add(uint64(i), uint64(i))
		}
		base, _ := fb.Finish()
		m := NewFrom(base, nil)
		defer m.Close()
		run(b, m)
	})
	b.Run("SyncMap", func(b *testing.B) {
		s := &syncMap[uint64, uint64]{}
		for i := range n {
			s.Put(uint64(i), uint64(i))
		}
		run(b, s)
	})
	b.Run("ShardedRWMutexMap", func(b *testing.B) {
		s := newShardedMap[uint64, uint64]()
		for i := range n {
			s.Put(uint64(i), uint64(i))
		}
		run(b, s)
	})
}

// BenchmarkMixedString compares string-to-string maps (where native maps
// store two pointers per entry and TieredMap stores zero in its base).
func BenchmarkMixedString(b *testing.B) {
	n := min(benchKeys(b), 200_000)
	keys := make([]string, n)
	vals := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("user:%08d", i)
		vals[i] = fmt.Sprintf("payload-value-%08d", i)
	}
	run := func(b *testing.B, s store[string, string]) {
		var seq atomic.Uint64
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			rng := rand.New(rand.NewPCG(seq.Add(1), 3))
			for pb.Next() {
				idx := rng.IntN(n)
				switch r := rng.IntN(100); {
				case r < 95:
					s.Get(keys[idx])
				case r < 98:
					s.Put(keys[idx], vals[idx])
				default:
					s.Delete(keys[idx])
				}
			}
		})
	}
	b.Run("TieredMap", func(b *testing.B) {
		fb, _ := cnfrt.NewFrozenBuilder[string, string](n, nil)
		for i := range n {
			fb.Add(keys[i], vals[i])
		}
		base, _ := fb.Finish()
		m := NewFrom(base, nil)
		defer m.Close()
		run(b, m)
	})
	b.Run("SyncMap", func(b *testing.B) {
		s := &syncMap[string, string]{}
		for i := range n {
			s.Put(keys[i], vals[i])
		}
		run(b, s)
	})
	b.Run("ShardedRWMutexMap", func(b *testing.B) {
		s := newShardedMap[string, string]()
		for i := range n {
			s.Put(keys[i], vals[i])
		}
		run(b, s)
	})
}

// BenchmarkGC reports the duration of a forced collection with each map
// fully populated.
func BenchmarkGC(b *testing.B) {
	n := benchKeys(b)
	measure := func(b *testing.B, keep any) {
		runtime.GC()
		for b.Loop() {
			runtime.GC()
		}
		runtime.KeepAlive(keep)
	}
	b.Run("Uint64/TieredMap", func(b *testing.B) {
		fb, _ := cnfrt.NewFrozenBuilder[uint64, uint64](n, nil)
		for i := range n {
			fb.Add(uint64(i), uint64(i))
		}
		base, _ := fb.Finish()
		m := NewFrom(base, nil)
		defer m.Close()
		measure(b, m)
	})
	b.Run("Uint64/SyncMap", func(b *testing.B) {
		s := &syncMap[uint64, uint64]{}
		for i := range n {
			s.Put(uint64(i), uint64(i))
		}
		measure(b, s)
	})
	b.Run("Uint64/ShardedRWMutexMap", func(b *testing.B) {
		s := newShardedMap[uint64, uint64]()
		for i := range n {
			s.Put(uint64(i), uint64(i))
		}
		measure(b, s)
	})
	b.Run("String/TieredMap", func(b *testing.B) {
		fb, _ := cnfrt.NewFrozenBuilder[string, string](n, nil)
		for i := range n {
			fb.Add(fmt.Sprintf("k:%08d", i), fmt.Sprintf("v:%08d", i))
		}
		base, _ := fb.Finish()
		m := NewFrom(base, nil)
		defer m.Close()
		measure(b, m)
	})
	b.Run("String/SyncMap", func(b *testing.B) {
		s := &syncMap[string, string]{}
		for i := range n {
			s.Put(fmt.Sprintf("k:%08d", i), fmt.Sprintf("v:%08d", i))
		}
		measure(b, s)
	})
	b.Run("String/ShardedRWMutexMap", func(b *testing.B) {
		s := newShardedMap[string, string]()
		for i := range n {
			s.Put(fmt.Sprintf("k:%08d", i), fmt.Sprintf("v:%08d", i))
		}
		measure(b, s)
	})
}
