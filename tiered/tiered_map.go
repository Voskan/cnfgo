// Package tiered provides TieredMap, a concurrent read-mostly map that keeps
// the bulk of its entries in a pointer-free cnfrt.FrozenMap and absorbs
// point mutations in small sharded delta layers.
//
// # Structure
//
// A TieredMap publishes an immutable epoch through an atomic pointer:
//
//	active    a mutable layer of 64 mutex-protected shards
//	flushing  sealed layers, newest first, that are never modified again
//	base      a FrozenMap holding everything compacted so far
//
// Writers mutate the active layer. When it reaches the flush threshold,
// max(MinFlushEntries, base.Len()/FlushRatioDivisor), it is sealed and pushed
// onto flushing, and a background compactor merges the flushing layers into
// a new base. Readers consult active (skipped lock-free when clean, otherwise
// one shard read lock), then flushing (no locks), then base (no locks); the
// first layer that knows the key answers, so a deletion is a tombstone that
// shadows older layers.
//
// # Correctness
//
// Rotation holds rotateMu and every shard lock of the old active layer while
// it marks the shards sealed and publishes the next epoch. A writer that
// locked a shard before the rotation finished its mutation before the
// rotation could lock that shard; a writer that locks it afterwards observes
// sealed and retries on the published epoch. No write is lost. Sealed
// layers are never mutated, cleared or reused, so they are read without
// locks. Tombstones survive merges between delta layers and are dropped only
// when merged into the base, so deleted keys are never resurrected.
//
// If MaxFlushing layers are waiting for compaction, the writer that triggers
// the next rotation blocks until the compactor catches up, bounding memory.
// If a compaction fails, writers are no longer throttled, the layers are
// kept (reads stay correct) and Flush and LastError report the error until a
// subsequent compaction succeeds.
//
// # Keys containing NaN
//
// A floating-point NaN is not equal to itself, so a key containing one could
// never be read back and cannot be stored in the base. Put and Delete ignore
// such keys and TryPut rejects them with cnfrt.ErrNaNKey.
package tiered

import (
	"hash/maphash"
	"iter"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/Voskan/cnfgo/cnfrt"
)

const numShards = 64

// Default configuration values.
const (
	DefaultMinFlushEntries   = 4096
	DefaultFlushRatioDivisor = 16
	DefaultMaxFlushing       = 4
)

// Options configures a TieredMap.
type Options[V any] struct {
	// MinFlushEntries is the minimum number of keys in the active layer that
	// triggers a rotation. Zero selects DefaultMinFlushEntries.
	MinFlushEntries int
	// FlushRatioDivisor sets the rotation threshold relative to the base:
	// base.Len()/FlushRatioDivisor. Zero selects DefaultFlushRatioDivisor.
	FlushRatioDivisor int
	// MaxFlushing is the number of sealed layers that may await compaction
	// before writers are throttled. Zero selects DefaultMaxFlushing.
	MaxFlushing int
	// Map configures the base FrozenMap (seed and value codec).
	Map cnfrt.MapOptions[V]
}

type entryState uint8

const (
	stateMissing entryState = iota
	stateValue
	stateTombstone
)

type deltaEntry[V any] struct {
	state entryState
	val   V
}

type deltaShard[K comparable, V any] struct {
	mu      sync.RWMutex
	sealed  bool
	entries map[K]deltaEntry[V]
	_       [88]byte // pad to 128 bytes so neighboring shard locks do not share a cache line
}

type deltaLayer[K comparable, V any] struct {
	shards [numShards]deltaShard[K, V]
	n      atomic.Int64 // number of distinct keys in the layer
	delta  atomic.Int64 // approximate net live-entry delta (+1 new value, -1 tombstone)
	// dirty is set, under the lock of the shard written, before the first
	// key becomes visible in the layer.
	dirty atomic.Bool
}

func newLayer[K comparable, V any]() *deltaLayer[K, V] {
	l := &deltaLayer[K, V]{}
	for i := range l.shards {
		l.shards[i].entries = make(map[K]deltaEntry[V])
	}
	return l
}

// get looks k up in a sealed layer without locking.
func (l *deltaLayer[K, V]) get(sh int, k K) (deltaEntry[V], bool) {
	e, ok := l.shards[uint(sh)&(numShards-1)].entries[k]
	return e, ok
}

type epoch[K comparable, V any] struct {
	active   *deltaLayer[K, V]
	flushing []*deltaLayer[K, V] // newest first
	base     *cnfrt.FrozenMap[K, V]
}

// TieredMap is a concurrent map optimized for reads, with a pointer-free
// base. The zero value is not usable; create maps with New or NewFrom.
type TieredMap[K comparable, V any] struct {
	c *core[K, V]
}

type core[K comparable, V any] struct {
	epoch atomic.Pointer[epoch[K, V]]
	seed  maphash.Seed
	opts  Options[V]

	rotateMu  sync.Mutex
	drained   *sync.Cond // signaled (with rotateMu) when flushing shrinks
	compactMu sync.Mutex

	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	closed atomic.Bool

	rotations   atomic.Int64
	compactions atomic.Int64
	err         atomic.Pointer[error]

	nan      func(K) bool           // nil if keys cannot contain NaN
	fallible cnfrt.FallibleCodec[V] // non-nil when value encoding can fail
}

// New returns an empty TieredMap.
func New[K comparable, V any](o *Options[V]) (*TieredMap[K, V], error) {
	var opts Options[V]
	if o != nil {
		opts = *o
	}
	base, err := cnfrt.EmptyFrozenMap[K, V](&opts.Map)
	if err != nil {
		return nil, err
	}
	return newMap(base, opts), nil
}

// NewFrom returns a TieredMap whose base is m. It always uses m's value codec
// (ignoring o.Map.Value), while o.Map.Seed and o.Map.DeterministicSeed
// configure future compactions.
func NewFrom[K comparable, V any](m *cnfrt.FrozenMap[K, V], o *Options[V]) *TieredMap[K, V] {
	var opts Options[V]
	if o != nil {
		opts = *o
	}
	opts.Map.Value = m.ValueCodec()
	return newMap(m, opts)
}

func newMap[K comparable, V any](base *cnfrt.FrozenMap[K, V], opts Options[V]) *TieredMap[K, V] {
	if opts.MinFlushEntries <= 0 {
		opts.MinFlushEntries = DefaultMinFlushEntries
	}
	if opts.FlushRatioDivisor <= 0 {
		opts.FlushRatioDivisor = DefaultFlushRatioDivisor
	}
	if opts.MaxFlushing <= 0 {
		opts.MaxFlushing = DefaultMaxFlushing
	}
	if opts.Map.Value == nil {
		opts.Map.Value = base.ValueCodec()
	}
	c := &core[K, V]{
		seed: maphash.MakeSeed(),
		opts: opts,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		nan:  cnfrt.NaNKeyFunc[K](),
	}
	if fc, ok := opts.Map.Value.(cnfrt.FallibleCodec[V]); ok {
		c.fallible = fc
	}
	c.drained = sync.NewCond(&c.rotateMu)
	c.epoch.Store(&epoch[K, V]{active: newLayer[K, V](), base: base})
	go c.compactLoop()
	m := &TieredMap[K, V]{c: c}
	// The compactor references only the core, so an unreachable map can be
	// collected; its cleanup stops the compactor.
	runtime.AddCleanup(m, func(c *core[K, V]) { c.close() }, c)
	return m
}

func (c *core[K, V]) shard(k K) int {
	return int(maphash.Comparable(c.seed, k) & (numShards - 1))
}

func (c *core[K, V]) threshold(ep *epoch[K, V]) int64 {
	return int64(max(c.opts.MinFlushEntries, ep.base.Len()/c.opts.FlushRatioDivisor))
}

// Get returns the value stored for k.
func (m *TieredMap[K, V]) Get(k K) (V, bool) {
	c := m.c
	ep := c.epoch.Load()
	activeDirty := ep.active.dirty.Load()
	if !activeDirty && len(ep.flushing) == 0 {
		return ep.base.Get(k)
	}
	return c.getSlow(ep, activeDirty, k)
}

func (c *core[K, V]) getSlow(ep *epoch[K, V], activeDirty bool, k K) (V, bool) {
	i := c.shard(k)
	var e deltaEntry[V]
	var ok bool
	if activeDirty {
		sh := &ep.active.shards[uint(i)&(numShards-1)]
		sh.mu.RLock()
		e, ok = sh.entries[k]
		sh.mu.RUnlock()
	}
	if !ok {
		for _, l := range ep.flushing {
			if e, ok = l.get(i, k); ok {
				break
			}
		}
	}
	if ok {
		if e.state == stateValue {
			return e.val, true
		}
		var zero V
		return zero, false
	}
	return ep.base.Get(k)
}

// Has reports whether k is present.
func (m *TieredMap[K, V]) Has(k K) bool {
	_, ok := m.Get(k)
	return ok
}

// Put stores v for k. Keys containing NaN and unencodable values are
// ignored; use TryPut to detect errors.
func (m *TieredMap[K, V]) Put(k K, v V) { _ = m.TryPut(k, v) }

// TryPut stores v for k. It returns cnfrt.ErrNaNKey if k contains a
// floating-point NaN, or any error returned by a FallibleCodec, without
// modifying the map.
func (m *TieredMap[K, V]) TryPut(k K, v V) error {
	c := m.c
	if c.nan != nil && c.nan(k) {
		return cnfrt.ErrNaNKey
	}
	if c.fallible != nil {
		var buf [256]byte
		if _, err := c.fallible.AppendErr(buf[:0], v); err != nil {
			return err
		}
	}
	c.write(k, deltaEntry[V]{state: stateValue, val: v})
	return nil
}

// Delete removes k. Keys containing NaN are never present, so deleting one
// does nothing.
func (m *TieredMap[K, V]) Delete(k K) {
	c := m.c
	if c.nan != nil && c.nan(k) {
		return
	}
	c.write(k, deltaEntry[V]{state: stateTombstone})
}

func (c *core[K, V]) write(k K, e deltaEntry[V]) {
	i := c.shard(k)
	for {
		ep := c.epoch.Load()
		l := ep.active
		sh := &l.shards[uint(i)&(numShards-1)]
		sh.mu.Lock()
		if sh.sealed {
			sh.mu.Unlock()
			continue
		}
		// Mark the layer dirty before releasing the shard lock so that any
		// reader or Snapshot starting after this write becomes visible sees
		// dirty == true (or a later epoch).
		if !l.dirty.Load() {
			l.dirty.Store(true)
		}
		prev, existed := sh.entries[k]
		sh.entries[k] = e
		sh.mu.Unlock()
		var d int64
		if !existed {
			if e.state == stateValue {
				d = 1
			} else {
				d = -1
			}
		} else if prev.state != e.state {
			if e.state == stateValue {
				d = 1
			} else {
				d = -1
			}
		}
		if d != 0 {
			l.delta.Add(d)
		}
		if !existed && l.n.Add(1) >= c.threshold(ep) {
			c.rotate(l, true)
		}
		return
	}
}

// rotate seals layer l (if it is still the active layer) and publishes a
// new epoch with l at the front of flushing. With wait set it applies
// backpressure when too many layers await compaction.
func (c *core[K, V]) rotate(l *deltaLayer[K, V], wait bool) {
	c.rotateMu.Lock()
	defer c.rotateMu.Unlock()
	for wait && len(c.epoch.Load().flushing) >= c.opts.MaxFlushing && c.epoch.Load().active == l && c.err.Load() == nil {
		c.signal()
		if c.closed.Load() {
			// No compactor is running: compact inline.
			c.rotateMu.Unlock()
			c.compact()
			c.rotateMu.Lock()
			continue
		}
		c.drained.Wait()
	}
	cur := c.epoch.Load()
	if cur.active != l {
		return // rotated by someone else
	}
	for i := range l.shards {
		l.shards[i].mu.Lock()
	}
	for i := range l.shards {
		l.shards[i].sealed = true
	}
	flushing := make([]*deltaLayer[K, V], 0, len(cur.flushing)+1)
	flushing = append(flushing, l)
	flushing = append(flushing, cur.flushing...)
	c.epoch.Store(&epoch[K, V]{active: newLayer[K, V](), flushing: flushing, base: cur.base})
	for i := range l.shards {
		l.shards[i].mu.Unlock()
	}
	c.rotations.Add(1)
	c.signal()
}

func (c *core[K, V]) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *core[K, V]) compactLoop() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			return
		case <-c.wake:
			c.compact()
		}
	}
}

// compact merges all layers currently awaiting compaction into a new base.
func (c *core[K, V]) compact() {
	c.compactMu.Lock()
	defer c.compactMu.Unlock()
	snap := c.epoch.Load()
	batch := snap.flushing
	if len(batch) == 0 {
		return
	}
	base, err := c.merge(snap.base, batch)
	if err != nil {
		// Keep the layers; reads remain correct and the error is reported
		// by Flush and LastError. Wake throttled writers, which stop
		// waiting once an error is recorded.
		c.rotateMu.Lock()
		c.err.Store(&err)
		c.drained.Broadcast()
		c.rotateMu.Unlock()
		return
	}
	c.rotateMu.Lock()
	cur := c.epoch.Load()
	// Rotations since the snapshot only prepended layers, so the batch is
	// the suffix of cur.flushing.
	keep := cur.flushing[:len(cur.flushing)-len(batch)]
	c.epoch.Store(&epoch[K, V]{active: cur.active, flushing: append([]*deltaLayer[K, V](nil), keep...), base: base})
	c.err.Store(nil)
	c.drained.Broadcast()
	c.rotateMu.Unlock()
	c.compactions.Add(1)
}

// merge builds a base from base and the sealed layers (newest first).
// Tombstones are applied, and therefore dropped, here.
func (c *core[K, V]) merge(base *cnfrt.FrozenMap[K, V], layers []*deltaLayer[K, V]) (*cnfrt.FrozenMap[K, V], error) {
	overlay := make(map[K]deltaEntry[V])
	for i := len(layers) - 1; i >= 0; i-- { // oldest first, newer layers win
		for s := range layers[i].shards {
			for k, e := range layers[i].shards[s].entries {
				overlay[k] = e
			}
		}
	}
	b, err := cnfrt.NewFrozenBuilder[K, V](base.Len()+len(overlay), &c.opts.Map)
	if err != nil {
		return nil, err
	}
	b.GrowData(len(base.Sections().Data))
	for rk, rv := range base.RawEntries() {
		k, err := base.DecodeKey(rk)
		if err != nil {
			return nil, err
		}
		if _, shadowed := overlay[k]; shadowed {
			continue
		}
		if err := b.AddRaw(rk, rv); err != nil {
			return nil, err
		}
	}
	for k, e := range overlay {
		if e.state != stateValue {
			continue
		}
		if err := b.Add(k, e.val); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// Flush seals the active layer and compacts everything into the base before
// returning. It returns the error from compaction, if any. Compaction
// rebuilds the base, so a Flush costs O(|base| + |delta|) time and memory
// regardless of how few keys changed; prefer the automatic rotation
// thresholds over frequent explicit flushes on large maps.
func (m *TieredMap[K, V]) Flush() error {
	c := m.c
	if ep := c.epoch.Load(); ep.active.dirty.Load() {
		c.rotate(ep.active, false)
	}
	c.compact()
	if p := c.err.Load(); p != nil {
		return *p
	}
	return nil
}

// LastError returns the error recorded by the most recent failed compaction,
// or nil if no compaction error is active.
func (m *TieredMap[K, V]) LastError() error {
	if p := m.c.err.Load(); p != nil {
		return *p
	}
	return nil
}

// Close stops the background compactor. The map remains usable; later
// rotations compact synchronously.
func (m *TieredMap[K, V]) Close() { m.c.close() }

func (c *core[K, V]) close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.stop)
		<-c.done
		c.rotateMu.Lock()
		c.drained.Broadcast()
		c.rotateMu.Unlock()
	}
}

// View is an immutable point-in-time view of a TieredMap.
type View[K comparable, V any] struct {
	layers []*deltaLayer[K, V] // newest first
	base   *cnfrt.FrozenMap[K, V]
	shard  func(K) int
}

// Snapshot returns a consistent point-in-time view of the map. Later
// mutations do not affect the view. Snapshot captures the active layer under
// shared shard read locks without rotating the active layer or triggering
// background compaction, preserving MaxFlushing bounds.
func (m *TieredMap[K, V]) Snapshot() *View[K, V] {
	c := m.c
	for {
		ep := c.epoch.Load()
		if !ep.active.dirty.Load() {
			return &View[K, V]{layers: ep.flushing, base: ep.base, shard: c.shard}
		}
		l := ep.active
		sealed := false
		locked := 0
		for i := range l.shards {
			l.shards[i].mu.RLock()
			locked = i + 1
			if l.shards[i].sealed {
				sealed = true
				break
			}
		}
		if sealed {
			for i := range locked {
				l.shards[i].mu.RUnlock()
			}
			continue
		}
		var snap *deltaLayer[K, V]
		for i := range l.shards {
			src := l.shards[i].entries
			if len(src) == 0 {
				continue
			}
			if snap == nil {
				snap = &deltaLayer[K, V]{}
			}
			dst := make(map[K]deltaEntry[V], len(src))
			for k, e := range src {
				dst[k] = e
			}
			snap.shards[i].entries = dst
		}
		for i := range l.shards {
			l.shards[i].mu.RUnlock()
		}
		if snap == nil {
			return &View[K, V]{layers: ep.flushing, base: ep.base, shard: c.shard}
		}
		layers := make([]*deltaLayer[K, V], 0, len(ep.flushing)+1)
		layers = append(layers, snap)
		layers = append(layers, ep.flushing...)
		return &View[K, V]{layers: layers, base: ep.base, shard: c.shard}
	}
}

// Get returns the value stored for k in the view.
func (v *View[K, V]) Get(k K) (V, bool) {
	i := v.shard(k)
	for _, l := range v.layers {
		if e, ok := l.get(i, k); ok {
			if e.state == stateValue {
				return e.val, true
			}
			var zero V
			return zero, false
		}
	}
	return v.base.Get(k)
}

// All returns an iterator over the entries of the view in unspecified order.
func (v *View[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		seen := make(map[K]struct{})
		for _, l := range v.layers {
			for s := range l.shards {
				for k, e := range l.shards[s].entries {
					if _, dup := seen[k]; dup {
						continue
					}
					seen[k] = struct{}{}
					if e.state == stateValue && !yield(k, e.val) {
						return
					}
				}
			}
		}
		for k, val := range v.base.All() {
			if _, shadowed := seen[k]; shadowed {
				continue
			}
			if !yield(k, val) {
				return
			}
		}
	}
}

// Len returns the number of entries in the view. It takes time
// proportional to the number of keys in the delta layers.
func (v *View[K, V]) Len() int {
	n := v.base.Len()
	seen := make(map[K]struct{})
	for _, l := range v.layers {
		for s := range l.shards {
			for k, e := range l.shards[s].entries {
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				inBase := v.base.Has(k)
				switch {
				case e.state == stateValue && !inBase:
					n++
				case e.state == stateTombstone && inBase:
					n--
				}
			}
		}
	}
	return n
}

// Range calls fn for each entry of a consistent snapshot of the map until fn
// returns false.
func (m *TieredMap[K, V]) Range(fn func(K, V) bool) {
	for k, v := range m.Snapshot().All() {
		if !fn(k, v) {
			return
		}
	}
}

// Len returns the number of entries of a consistent snapshot of the map.
func (m *TieredMap[K, V]) Len() int { return m.Snapshot().Len() }

// ApproxLen returns an approximate entry count in O(1) time without locking,
// rotating, or allocating. The result equals the exact count after Flush and
// whenever delta layers contain no overwrites of base keys or deletions of
// absent keys.
func (m *TieredMap[K, V]) ApproxLen() int {
	ep := m.c.epoch.Load()
	n := int64(ep.base.Len()) + ep.active.delta.Load()
	for _, l := range ep.flushing {
		n += l.delta.Load()
	}
	if n < 0 {
		return 0
	}
	return int(n)
}

// Base returns the current base map, which holds every entry compacted so
// far. Entries in delta layers are not included; call Flush first to fold
// them in.
func (m *TieredMap[K, V]) Base() *cnfrt.FrozenMap[K, V] { return m.c.epoch.Load().base }

// Stats describes the internal state of a TieredMap.
type Stats struct {
	ActiveKeys  int64 // keys in the active layer
	Flushing    int   // sealed layers awaiting compaction
	BaseLen     int   // entries in the base
	Rotations   int64 // completed rotations
	Compactions int64 // completed compactions
}

// Stats returns a snapshot of internal counters.
func (m *TieredMap[K, V]) Stats() Stats {
	c := m.c
	ep := c.epoch.Load()
	return Stats{
		ActiveKeys:  ep.active.n.Load(),
		Flushing:    len(ep.flushing),
		BaseLen:     ep.base.Len(),
		Rotations:   c.rotations.Load(),
		Compactions: c.compactions.Load(),
	}
}
