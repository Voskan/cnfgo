package cnfrt

import (
	"encoding/binary"
	"iter"
	"math"
	"sync"
)

// DefaultSlabSize is the default upper bound of the slab size of a SlabMap
// shard. A shard starts with a 4 KiB slab and doubles the size of each new
// slab up to this bound, so small maps stay small.
const DefaultSlabSize = 256 << 10

const (
	slabShards     = 64
	slabShardShift = 64 - 6
	minSlabSize    = 4 << 10
)

// slabSlot locates the head record of a hash chain. It is pointer-free, so
// the buckets of the index map are allocated in noscan spans.
type slabSlot struct {
	slab uint32
	off  uint32
}

const (
	recHeader = 16
	nilSlab   = math.MaxUint32
	deadBit   = 1 << 31
)

type slabInfo struct {
	used    uint32
	dead    uint32
	queued  bool
	present bool
}

// SlabStats describes the storage of a SlabMap.
type SlabStats struct {
	Len       int
	Slabs     int
	LiveBytes uint64
	DeadBytes uint64
}

// SlabMap is a mutable hash map whose index and storage contain no Go
// pointers apart from one slice header per slab. Keys and values are
// encoded into byte slabs; the index is a map[uint64]slabSlot keyed by the
// 64-bit key hash, whose buckets the collector never scans. Each record
// stores the full key encoding, and records whose keys share a hash are
// chained, so hash collisions are resolved exactly.
//
// The map is split into 64 shards selected by the top bits of the key hash.
// Each shard has its own lock, index and slabs, so operations on different
// shards proceed in parallel.
//
// Overwritten and deleted records become dead space. When more than half of
// a sealed slab's written bytes are dead it is compacted in the background:
// its live records are moved to the shard's current slab and the slab is
// released. Compaction locks only the shard that owns the slab, one slab at
// a time.
//
// A SlabMap is safe for concurrent use. Get returns decoded copies that
// never alias the slabs; Borrow gives zero-copy access to the encoded value
// under the shard's read lock.
type SlabMap[K comparable, V any] struct {
	shards [slabShards]slabShard

	seed     uint64
	hashMask uint64
	kc       *keyCodec[K]
	vc       Codec[V]
}

// slabShard is one independently locked part of a SlabMap. Its methods
// operate on encoded keys and values only. It is padded to 256 bytes (a
// multiple of 128-byte ARM64/Apple Silicon cache lines) so neighboring
// shard locks never share a cache line.
type slabShard struct {
	mu       sync.RWMutex
	index    map[uint64]slabSlot
	slabs    [][]byte
	info     []slabInfo
	free     []uint32
	active   uint32
	valign   uint32
	nextSize int
	n        int
	live     uint64
	deadSum  uint64

	pending    []uint32
	compacting bool

	maxSize int
	idle    *sync.Cond
	hash    func(key string) uint64
	scratch []byte   // reusable value encoding buffer under mu
	_       [32]byte // pad to 256 bytes on 64-bit platforms
}

// NewSlabMap returns an empty SlabMap.
func NewSlabMap[K comparable, V any](o *MapOptions[V]) (*SlabMap[K, V], error) {
	return NewSlabMapSize[K, V](DefaultSlabSize, o)
}

// NewSlabMapSize returns an empty SlabMap whose shards use slabs of at most
// slabSize bytes. Sizes below 4 KiB or above MaxBlob select
// DefaultSlabSize. A record larger than slabSize gets a slab of its own.
func NewSlabMapSize[K comparable, V any](slabSize int, o *MapOptions[V]) (*SlabMap[K, V], error) {
	seed, kc, vc, err := resolveOptions[K, V](o)
	if err != nil {
		return nil, err
	}
	if slabSize < minSlabSize || slabSize > MaxBlob {
		slabSize = DefaultSlabSize
	}
	kc.clone = true
	vc = withClone(vc)
	valign := uint32(valueAlign(vc))
	m := &SlabMap[K, V]{seed: seed, hashMask: ^uint64(0), kc: kc, vc: vc}
	for i := range m.shards {
		sh := &m.shards[i]
		sh.index = make(map[uint64]slabSlot)
		sh.active = nilSlab
		sh.valign = valign
		sh.maxSize = slabSize
		sh.nextSize = minSlabSize
		sh.idle = sync.NewCond(&sh.mu)
		sh.hash = m.hash
	}
	return m, nil
}

// hash returns the index key of an encoded key. hashMask is all ones except
// in tests that force collisions.
func (m *SlabMap[K, V]) hash(key string) uint64 { return hashKey(key, m.seed) & m.hashMask }

// hashKey is HashString behind a non-inlined concrete call. Calls from
// generic methods would otherwise route through a dictionary, preventing
// escape analysis from keeping stack key buffers off the heap.
//
//go:noinline
func hashKey(key string, seed uint64) uint64 { return HashString(key, seed) }

func (m *SlabMap[K, V]) shard(h uint64) *slabShard { return &m.shards[h>>slabShardShift] }

func (m *slabShard) hdr(s, off uint32) []byte { return m.slabs[s][off : off+recHeader] }

func (m *slabShard) next(s, off uint32) (uint32, uint32) {
	h := m.hdr(s, off)
	_ = h[7]
	return binary.LittleEndian.Uint32(h[0:4]), binary.LittleEndian.Uint32(h[4:8])
}

func (m *slabShard) setNext(s, off, ns, noff uint32) {
	h := m.hdr(s, off)
	_ = h[7]
	binary.LittleEndian.PutUint32(h[0:4], ns)
	binary.LittleEndian.PutUint32(h[4:8], noff)
}

func (m *slabShard) lens(s, off uint32) (uint32, uint32) {
	h := m.hdr(s, off)
	_ = h[15]
	return binary.LittleEndian.Uint32(h[8:12]), binary.LittleEndian.Uint32(h[12:16]) &^ deadBit
}

func (m *slabShard) keyAt(s, off uint32) []byte {
	rec := m.slabs[s][off:]
	_ = rec[11]
	kl := binary.LittleEndian.Uint32(rec[8:12])
	return rec[recHeader : recHeader+kl]
}

func (m *slabShard) valRel(kl uint32) uint32 {
	mask := m.valign - 1
	return (recHeader + kl + mask) &^ mask
}

func (m *slabShard) recSize(kl, vl uint32) uint32 {
	mask := m.valign - 1
	voff := (recHeader + kl + mask) &^ mask
	return (voff + vl + mask) &^ mask
}

func (m *slabShard) valAt(s, off uint32) []byte {
	rec := m.slabs[s][off:]
	_ = rec[15]
	kl := binary.LittleEndian.Uint32(rec[8:12])
	vl := binary.LittleEndian.Uint32(rec[12:16]) &^ deadBit
	start := m.valRel(kl)
	return rec[start : start+vl]
}

// locateUint64 finds the record with 8-byte little-endian key v in the chain
// of hash h.
func (m *slabShard) locateUint64(h, v uint64) (s, off, ps, poff uint32, ok bool) {
	head, found := m.index[h]
	if !found {
		return 0, 0, nilSlab, 0, false
	}
	ps = nilSlab
	s, off = head.slab, head.off
	for s != nilSlab {
		rec := m.slabs[s][off:]
		_ = rec[11]
		if binary.LittleEndian.Uint32(rec[8:12]) == 8 && binary.LittleEndian.Uint64(rec[recHeader:recHeader+8]) == v {
			return s, off, ps, poff, true
		}
		ps, poff = s, off
		s = binary.LittleEndian.Uint32(rec[0:4])
		off = binary.LittleEndian.Uint32(rec[4:8])
	}
	return 0, 0, nilSlab, 0, false
}

// locateUint32 finds the record with 4-byte little-endian key v in the chain
// of hash h.
func (m *slabShard) locateUint32(h uint64, v uint32) (s, off, ps, poff uint32, ok bool) {
	head, found := m.index[h]
	if !found {
		return 0, 0, nilSlab, 0, false
	}
	ps = nilSlab
	s, off = head.slab, head.off
	for s != nilSlab {
		rec := m.slabs[s][off:]
		_ = rec[11]
		if binary.LittleEndian.Uint32(rec[8:12]) == 4 && binary.LittleEndian.Uint32(rec[recHeader:recHeader+4]) == v {
			return s, off, ps, poff, true
		}
		ps, poff = s, off
		s = binary.LittleEndian.Uint32(rec[0:4])
		off = binary.LittleEndian.Uint32(rec[4:8])
	}
	return 0, 0, nilSlab, 0, false
}

// locate finds the record with key bytes key in the chain of hash h. It
// returns the record and its predecessor (nilSlab for the chain head).
func (m *slabShard) locate(h uint64, key string) (s, off, ps, poff uint32, ok bool) {
	head, found := m.index[h]
	if !found {
		return 0, 0, nilSlab, 0, false
	}
	ps = nilSlab
	s, off = head.slab, head.off
	for s != nilSlab {
		rec := m.slabs[s][off:]
		_ = rec[11]
		kl := binary.LittleEndian.Uint32(rec[8:12])
		if string(rec[recHeader:recHeader+kl]) == key {
			return s, off, ps, poff, true
		}
		ps, poff = s, off
		s = binary.LittleEndian.Uint32(rec[0:4])
		off = binary.LittleEndian.Uint32(rec[4:8])
	}
	return 0, 0, nilSlab, 0, false
}

// relink points the predecessor link (or the index head) at (ns, noff).
func (m *slabShard) relink(h uint64, ps, poff, ns, noff uint32) {
	if ps == nilSlab {
		if ns == nilSlab {
			delete(m.index, h)
		} else {
			m.index[h] = slabSlot{slab: ns, off: noff}
		}
		return
	}
	m.setNext(ps, poff, ns, noff)
}

// alloc reserves size bytes for a record and returns its location.
func (m *slabShard) alloc(size int) (uint32, uint32) {
	if size > m.maxSize {
		id := m.newSlab(size)
		m.info[id].used = uint32(size)
		return id, 0
	}
	if m.active == nilSlab || int(m.info[m.active].used)+size > len(m.slabs[m.active]) {
		if m.active != nilSlab {
			m.maybeQueue(m.active, true)
		}
		for m.nextSize < size && m.nextSize < m.maxSize {
			if m.nextSize > m.maxSize/2 {
				m.nextSize = m.maxSize
			} else {
				m.nextSize *= 2
			}
		}
		m.active = m.newSlab(min(m.nextSize, m.maxSize))
		if m.nextSize > m.maxSize/2 {
			m.nextSize = m.maxSize
		} else {
			m.nextSize *= 2
		}
	}
	off := m.info[m.active].used
	m.info[m.active].used += uint32(size)
	return m.active, off
}

func (m *slabShard) newSlab(size int) uint32 {
	buf := make([]byte, size)
	if n := len(m.free); n > 0 {
		id := m.free[n-1]
		m.free = m.free[:n-1]
		m.slabs[id] = buf
		m.info[id] = slabInfo{present: true}
		return id
	}
	if uint64(len(m.slabs)) >= nilSlab-1 {
		panic("cnfrt: SlabMap slab count exhausted")
	}
	m.slabs = append(m.slabs, buf)
	m.info = append(m.info, slabInfo{present: true})
	return uint32(len(m.slabs) - 1)
}

func (m *slabShard) write(s, off uint32, key string, val []byte, ns, noff uint32) {
	voff := m.valRel(uint32(len(key)))
	b := m.slabs[s][off : off+voff+uint32(len(val))]
	_ = b[recHeader-1]
	binary.LittleEndian.PutUint32(b[0:4], ns)
	binary.LittleEndian.PutUint32(b[4:8], noff)
	binary.LittleEndian.PutUint32(b[8:12], uint32(len(key)))
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(val)))
	copy(b[recHeader:], key)
	copy(b[voff:], val)
}

func (m *slabShard) writeUint64(s, off uint32, key uint64, val []byte, ns, noff uint32) {
	voff := m.valRel(8)
	b := m.slabs[s][off : off+voff+uint32(len(val))]
	_ = b[recHeader+7]
	binary.LittleEndian.PutUint32(b[0:4], ns)
	binary.LittleEndian.PutUint32(b[4:8], noff)
	binary.LittleEndian.PutUint32(b[8:12], 8)
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(val)))
	binary.LittleEndian.PutUint64(b[recHeader:recHeader+8], key)
	copy(b[voff:], val)
}

func (m *slabShard) writeUint32(s, off uint32, key uint32, val []byte, ns, noff uint32) {
	voff := m.valRel(4)
	b := m.slabs[s][off : off+voff+uint32(len(val))]
	_ = b[recHeader+3]
	binary.LittleEndian.PutUint32(b[0:4], ns)
	binary.LittleEndian.PutUint32(b[4:8], noff)
	binary.LittleEndian.PutUint32(b[8:12], 4)
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(val)))
	binary.LittleEndian.PutUint32(b[recHeader:recHeader+4], key)
	copy(b[voff:], val)
}

func (m *slabShard) putUint64Locked(h, key uint64, val []byte) {
	size := m.recSize(8, uint32(len(val)))
	s, off, ps, poff, ok := m.locateUint64(h, key)
	ts, toff := m.alloc(int(size))
	if ok {
		ns, noff := m.next(s, off)
		m.writeUint64(ts, toff, key, val, ns, noff)
		m.relink(h, ps, poff, ts, toff)
		m.kill(s, off)
	} else {
		ns, noff := uint32(nilSlab), uint32(0)
		if head, found := m.index[h]; found {
			ns, noff = head.slab, head.off
		}
		m.writeUint64(ts, toff, key, val, ns, noff)
		m.index[h] = slabSlot{slab: ts, off: toff}
		m.n++
	}
	m.live += uint64(size)
}

func (m *slabShard) putUint32Locked(h uint64, key uint32, val []byte) {
	size := m.recSize(4, uint32(len(val)))
	s, off, ps, poff, ok := m.locateUint32(h, key)
	ts, toff := m.alloc(int(size))
	if ok {
		ns, noff := m.next(s, off)
		m.writeUint32(ts, toff, key, val, ns, noff)
		m.relink(h, ps, poff, ts, toff)
		m.kill(s, off)
	} else {
		ns, noff := uint32(nilSlab), uint32(0)
		if head, found := m.index[h]; found {
			ns, noff = head.slab, head.off
		}
		m.writeUint32(ts, toff, key, val, ns, noff)
		m.index[h] = slabSlot{slab: ts, off: toff}
		m.n++
	}
	m.live += uint64(size)
}

// kill marks the record at (s, off) dead and schedules compaction or
// release of its slab.
func (m *slabShard) kill(s, off uint32) {
	kl, vl := m.lens(s, off)
	h := m.hdr(s, off)
	binary.LittleEndian.PutUint32(h[12:16], vl|deadBit)
	size := m.recSize(kl, vl)
	m.info[s].dead += size
	m.live -= uint64(size)
	m.deadSum += uint64(size)
	m.maybeQueue(s, false)
}

func (m *slabShard) release(s uint32) {
	m.deadSum -= uint64(m.info[s].dead)
	m.slabs[s] = nil
	m.info[s] = slabInfo{}
	m.free = append(m.free, s)
	if m.active == s {
		m.active = nilSlab
	}
}

func (m *slabShard) maybeQueue(s uint32, sealed bool) {
	in := &m.info[s]
	if !in.present || in.queued {
		return
	}
	if in.dead == in.used {
		if s != m.active || sealed {
			m.release(s)
			return
		}
		// Active slab has no live records left; rewind it in place.
		m.deadSum -= uint64(in.dead)
		in.used = 0
		in.dead = 0
		return
	}
	if s == m.active && !sealed {
		return
	}
	if uint64(in.dead) > uint64(in.used)/2 {
		in.queued = true
		m.pending = append(m.pending, s)
		if !m.compacting {
			m.compacting = true
			go m.compactLoop()
		}
	}
}

func (m *slabShard) compactLoop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.pending) > 0 {
		s := m.pending[0]
		m.pending = m.pending[1:]
		m.compactSlab(s)
		// Let readers and writers in between slabs.
		m.mu.Unlock()
		m.mu.Lock()
	}
	m.pending = nil
	m.compacting = false
	m.idle.Broadcast()
}

// compactSlab moves every live record of slab s to the active slab and
// releases s. The caller holds the write lock.
func (m *slabShard) compactSlab(s uint32) {
	in := &m.info[s]
	if !in.present {
		return
	}
	in.queued = false
	if m.active == s {
		m.active = nilSlab
	}
	for off := uint32(0); off < in.used; {
		kl, vl := m.lens(s, off)
		dead := binary.LittleEndian.Uint32(m.hdr(s, off)[12:16])&deadBit != 0
		size := m.recSize(kl, vl)
		if !dead {
			key := UnsafeSealBytes(m.keyAt(s, off))
			h := m.hash(key)
			_, _, ps, poff, ok := m.locate(h, key)
			if ok {
				ns, noff := m.next(s, off)
				ts, toff := m.alloc(int(size))
				copy(m.slabs[ts][toff:toff+size], m.slabs[s][off:off+size])
				m.setNext(ts, toff, ns, noff)
				m.relink(h, ps, poff, ts, toff)
			}
		}
		off += size
	}
	m.release(s)
}

// compact synchronously compacts every sealed slab of the shard whose dead
// space exceeds half of its written bytes, releases fully dead slabs, and
// waits for any background compaction to finish.
func (m *slabShard) compact() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		for m.compacting {
			m.idle.Wait()
		}
		didWork := false
		for s := range m.info {
			in := &m.info[s]
			if !in.present {
				continue
			}
			if in.dead == in.used {
				if uint32(s) == m.active {
					m.deadSum -= uint64(in.dead)
					in.used = 0
					in.dead = 0
				} else {
					m.release(uint32(s))
				}
				continue
			}
			if uint32(s) != m.active && uint64(in.dead) > uint64(in.used)/2 {
				m.compactSlab(uint32(s))
				didWork = true
			}
		}
		if !m.compacting && !didWork {
			return
		}
	}
}

// Compact synchronously compacts every slab whose dead space exceeds half
// of its written bytes and waits for background compaction to finish.
func (m *SlabMap[K, V]) Compact() {
	for i := range m.shards {
		m.shards[i].compact()
	}
}

func (m *SlabMap[K, V]) encodeKey(buf *keyBuf, k *K) (string, error) {
	if m.kc.isString {
		return m.kc.str(k), nil
	}
	return m.kc.encode(buf, k)
}

// Put stores v under k. It returns ErrNaNKey for keys containing NaN, or any
// encoding error returned by a FallibleCodec.
func (m *SlabMap[K, V]) Put(k K, v V) error {
	switch m.kc.fast {
	case keyFixed8:
		kv := m.kc.u64(&k)
		h := HashUint64(kv, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		val, err := AppendValue(m.vc, sh.scratch[:0], v)
		if cap(val) <= 4096 {
			sh.scratch = val[:0]
		} else {
			sh.scratch = nil
		}
		if err != nil {
			return err
		}
		if uint64(len(val)) > MaxBlob/2 {
			return ErrCapacityExceeded
		}
		sh.putUint64Locked(h, kv, val)
		return nil
	case keyFixed4:
		kv := m.kc.u32(&k)
		h := HashUint32(kv, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		val, err := AppendValue(m.vc, sh.scratch[:0], v)
		if cap(val) <= 4096 {
			sh.scratch = val[:0]
		} else {
			sh.scratch = nil
		}
		if err != nil {
			return err
		}
		if uint64(len(val)) > MaxBlob/2 {
			return ErrCapacityExceeded
		}
		sh.putUint32Locked(h, kv, val)
		return nil
	}
	var buf keyBuf
	key, err := m.encodeKey(&buf, &k)
	if err != nil {
		return err
	}
	if uint64(len(key)) > MaxBlob/2 {
		return ErrCapacityExceeded
	}
	h := m.hash(key)
	sh := m.shard(h)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	val, err := AppendValue(m.vc, sh.scratch[:0], v)
	if cap(val) <= 4096 {
		sh.scratch = val[:0]
	} else {
		sh.scratch = nil
	}
	if err != nil {
		return err
	}
	if uint64(len(val)) > MaxBlob/2 {
		return ErrCapacityExceeded
	}
	size := sh.recSize(uint32(len(key)), uint32(len(val)))
	s, off, ps, poff, ok := sh.locate(h, key)
	ts, toff := sh.alloc(int(size))
	if ok {
		ns, noff := sh.next(s, off)
		sh.write(ts, toff, key, val, ns, noff)
		sh.relink(h, ps, poff, ts, toff)
		sh.kill(s, off)
	} else {
		ns, noff := uint32(nilSlab), uint32(0)
		if head, found := sh.index[h]; found {
			ns, noff = head.slab, head.off
		}
		sh.write(ts, toff, key, val, ns, noff)
		sh.index[h] = slabSlot{slab: ts, off: toff}
		sh.n++
	}
	sh.live += uint64(size)
	return nil
}

// Delete removes k and reports whether it was present.
func (m *SlabMap[K, V]) Delete(k K) bool {
	switch m.kc.fast {
	case keyFixed8:
		kv := m.kc.u64(&k)
		h := HashUint64(kv, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.Lock()
		s, off, ps, poff, ok := sh.locateUint64(h, kv)
		if !ok {
			sh.mu.Unlock()
			return false
		}
		ns, noff := sh.next(s, off)
		sh.relink(h, ps, poff, ns, noff)
		sh.kill(s, off)
		sh.n--
		sh.mu.Unlock()
		return true
	case keyFixed4:
		kv := m.kc.u32(&k)
		h := HashUint32(kv, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.Lock()
		s, off, ps, poff, ok := sh.locateUint32(h, kv)
		if !ok {
			sh.mu.Unlock()
			return false
		}
		ns, noff := sh.next(s, off)
		sh.relink(h, ps, poff, ns, noff)
		sh.kill(s, off)
		sh.n--
		sh.mu.Unlock()
		return true
	}
	var buf keyBuf
	key, err := m.encodeKey(&buf, &k)
	if err != nil {
		return false
	}
	h := m.hash(key)
	sh := m.shard(h)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	s, off, ps, poff, ok := sh.locate(h, key)
	if !ok {
		return false
	}
	ns, noff := sh.next(s, off)
	sh.relink(h, ps, poff, ns, noff)
	sh.kill(s, off)
	sh.n--
	return true
}

// Get returns a decoded copy of the value stored for k. The value is
// decoded under the shard's read lock; the map's codecs always copy, so the
// result never aliases a slab. If V is a dictionary-backed Compact value whose
// Dict has been closed, decoding fails and Get returns (zero, false); do not
// close a Dict while a SlabMap referencing it remains in use.
func (m *SlabMap[K, V]) Get(k K) (V, bool) {
	var zero V
	switch m.kc.fast {
	case keyFixed8:
		kv := m.kc.u64(&k)
		h := HashUint64(kv, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.RLock()
		s, off, _, _, ok := sh.locateUint64(h, kv)
		if !ok {
			sh.mu.RUnlock()
			return zero, false
		}
		v, err := m.vc.Decode(UnsafeSealBytes(sh.valAt(s, off)))
		sh.mu.RUnlock()
		if err != nil {
			return zero, false
		}
		return v, true
	case keyFixed4:
		kv := m.kc.u32(&k)
		h := HashUint32(kv, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.RLock()
		s, off, _, _, ok := sh.locateUint32(h, kv)
		if !ok {
			sh.mu.RUnlock()
			return zero, false
		}
		v, err := m.vc.Decode(UnsafeSealBytes(sh.valAt(s, off)))
		sh.mu.RUnlock()
		if err != nil {
			return zero, false
		}
		return v, true
	case keyString:
		key := m.kc.str(&k)
		h := HashString(key, m.seed) & m.hashMask
		sh := m.shard(h)
		sh.mu.RLock()
		s, off, _, _, ok := sh.locate(h, key)
		if !ok {
			sh.mu.RUnlock()
			return zero, false
		}
		v, err := m.vc.Decode(UnsafeSealBytes(sh.valAt(s, off)))
		sh.mu.RUnlock()
		if err != nil {
			return zero, false
		}
		return v, true
	}
	var buf keyBuf
	if m.kc.hasNaN(&k) {
		return zero, false
	}
	key, err := m.encodeKey(&buf, &k)
	if err != nil {
		return zero, false
	}
	h := m.hash(key)
	sh := m.shard(h)
	sh.mu.RLock()
	s, off, _, _, ok := sh.locate(h, key)
	if !ok {
		sh.mu.RUnlock()
		return zero, false
	}
	v, err := m.vc.Decode(UnsafeSealBytes(sh.valAt(s, off)))
	sh.mu.RUnlock()
	if err != nil {
		return zero, false
	}
	return v, true
}

func (m *slabShard) borrowUint64(h, v uint64, fn func(val []byte)) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, off, _, _, ok := m.locateUint64(h, v)
	if !ok {
		return false
	}
	fn(m.valAt(s, off))
	return true
}

func (m *slabShard) borrowUint32(h uint64, v uint32, fn func(val []byte)) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, off, _, _, ok := m.locateUint32(h, v)
	if !ok {
		return false
	}
	fn(m.valAt(s, off))
	return true
}

func (m *slabShard) borrowString(h uint64, key string, fn func(val []byte)) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, off, _, _, ok := m.locate(h, key)
	if !ok {
		return false
	}
	fn(m.valAt(s, off))
	return true
}

// Borrow calls fn with the encoded value stored for k while holding the
// read lock of k's shard. fn must not retain the slice or call methods of m.
func (m *SlabMap[K, V]) Borrow(k K, fn func(val []byte)) bool {
	switch m.kc.fast {
	case keyFixed8:
		kv := m.kc.u64(&k)
		h := HashUint64(kv, m.seed) & m.hashMask
		return m.shard(h).borrowUint64(h, kv, fn)
	case keyFixed4:
		kv := m.kc.u32(&k)
		h := HashUint32(kv, m.seed) & m.hashMask
		return m.shard(h).borrowUint32(h, kv, fn)
	case keyString:
		key := m.kc.str(&k)
		h := HashString(key, m.seed) & m.hashMask
		return m.shard(h).borrowString(h, key, fn)
	}
	var buf keyBuf
	if m.kc.hasNaN(&k) {
		return false
	}
	key, err := m.encodeKey(&buf, &k)
	if err != nil {
		return false
	}
	h := m.hash(key)
	return m.shard(h).borrowString(h, key, fn)
}

// Has reports whether k is present.
func (m *SlabMap[K, V]) Has(k K) bool { return m.Borrow(k, func([]byte) {}) }

// Len returns the number of entries.
func (m *SlabMap[K, V]) Len() int {
	n := 0
	for i := range m.shards {
		sh := &m.shards[i]
		sh.mu.RLock()
		n += sh.n
		sh.mu.RUnlock()
	}
	return n
}

// Stats returns storage statistics.
func (m *SlabMap[K, V]) Stats() SlabStats {
	var st SlabStats
	for i := range m.shards {
		sh := &m.shards[i]
		sh.mu.RLock()
		st.Len += sh.n
		st.LiveBytes += sh.live
		st.DeadBytes += sh.deadSum
		for _, in := range sh.info {
			if in.present {
				st.Slabs++
			}
		}
		sh.mu.RUnlock()
	}
	return st
}

// All returns an iterator over decoded copies of all entries. Each shard is
// snapshotted under its read lock and unlocked before yielding, so the loop
// body may safely call methods on m (including Put and Delete).
func (m *SlabMap[K, V]) All() iter.Seq2[K, V] {
	type kv struct {
		k K
		v V
	}
	return func(yield func(K, V) bool) {
		var batch []kv
		for i := range m.shards {
			sh := &m.shards[i]
			batch = batch[:0]
			sh.mu.RLock()
			if sh.n > 0 {
				if cap(batch) < sh.n {
					batch = make([]kv, 0, sh.n)
				}
				for _, head := range sh.index {
					for s, off := head.slab, head.off; s != nilSlab; s, off = sh.next(s, off) {
						k, err := m.kc.decode(UnsafeSealBytes(sh.keyAt(s, off)))
						if err != nil {
							continue
						}
						v, err := m.vc.Decode(UnsafeSealBytes(sh.valAt(s, off)))
						if err != nil {
							continue
						}
						batch = append(batch, kv{k: k, v: v})
					}
				}
			}
			sh.mu.RUnlock()
			for _, e := range batch {
				if !yield(e.k, e.v) {
					return
				}
			}
		}
	}
}
