package cnfrt

import (
	"math/bits"
	"time"
)

// FreezeMode selects how a source object graph is traversed.
type FreezeMode uint8

const (
	// ModeTree treats the source as a tree: no visited set is kept, shared
	// pointers are duplicated and recursion deeper than the maximum depth
	// fails with ErrCycleDetected.
	ModeTree FreezeMode = iota
	// ModeDAG deduplicates shared pointers using a pointer-free visited
	// table and rejects directed cycles with ErrCycleDetected. Node IDs are
	// assigned in topological order: every child reference within a table
	// has a larger ID than its parent.
	ModeDAG
	// ModeCyclic deduplicates shared pointers and permits cycles. Snapshots
	// produced in this mode carry FlagCyclicAllowed.
	ModeCyclic
)

// Traversal depth limits. Generated Freeze functions recurse once per level
// of the source graph, so the limits keep the goroutine stack well below
// the runtime maximum on 32-bit and 64-bit platforms; deeper graphs fail
// with ErrCapacityExceeded (ErrCycleDetected in ModeTree) instead of
// overflowing the stack.
const (
	DefaultTreeMaxDepth  = 4096
	DefaultGraphMaxDepth = 1 << 15
	// MaxGraphDepth is the upper bound applied to FreezeOptions.MaxDepth.
	MaxGraphDepth = 1 << 16
)

// FreezeOptions configures a Freeze operation.
type FreezeOptions struct {
	// MaxDepth bounds the recursion depth of the traversal. Zero selects
	// DefaultTreeMaxDepth for ModeTree and DefaultGraphMaxDepth otherwise;
	// larger values are capped at MaxGraphDepth.
	MaxDepth int
	// PreserveLocal encodes time.Local as the location "Local" instead of
	// the fixed zone in effect at each instant.
	PreserveLocal bool
	// ExactStringDedup deduplicates all strings exactly using a temporary
	// map instead of the bounded pointer-free interning cache.
	ExactStringDedup bool
	// NoStringDedup disables string deduplication.
	NoStringDedup bool
	// HashSeed is the seed of hash-indexed maps; zero selects a random seed
	// unless DeterministicSeed is set, in which case DefaultSeed is used.
	HashSeed uint64
	// DeterministicSeed selects DefaultSeed when HashSeed is zero, producing
	// byte-for-byte reproducible snapshots.
	DeterministicSeed bool
	// NodeHint is the expected number of distinct pointer-referenced nodes
	// in ModeDAG or ModeCyclic. When positive, the visited table is
	// pre-sized to avoid repeated growth during the first pass.
	NodeHint int
	// Dict, if non-nil, deduplicates repeated strings across independent
	// Pack and Freeze calls into a shared pointer-free dictionary.
	// Dict-backed snapshots and compact values are intended for in-memory
	// caches; standalone .cnf serialization requires detaching Compact values
	// with Detach (or rebuilding dict-backed snapshots with Unfreeze and
	// Freeze). Freezing with a closed Dict fails with ErrUnknownDict.
	Dict *Dict
}

// FreezeOption configures FreezeOptions.
type FreezeOption func(*FreezeOptions)

// WithMaxDepth sets FreezeOptions.MaxDepth.
func WithMaxDepth(n int) FreezeOption { return func(o *FreezeOptions) { o.MaxDepth = n } }

// WithPreserveLocal sets FreezeOptions.PreserveLocal.
func WithPreserveLocal() FreezeOption { return func(o *FreezeOptions) { o.PreserveLocal = true } }

// WithExactStringDedup sets FreezeOptions.ExactStringDedup.
func WithExactStringDedup() FreezeOption {
	return func(o *FreezeOptions) { o.ExactStringDedup = true }
}

// WithoutStringDedup sets FreezeOptions.NoStringDedup.
func WithoutStringDedup() FreezeOption { return func(o *FreezeOptions) { o.NoStringDedup = true } }

// WithHashSeed sets FreezeOptions.HashSeed.
func WithHashSeed(seed uint64) FreezeOption { return func(o *FreezeOptions) { o.HashSeed = seed } }

// WithDeterministicSeed selects DefaultSeed when no explicit HashSeed is set,
// producing reproducible hash-indexed maps across runs.
func WithDeterministicSeed() FreezeOption {
	return func(o *FreezeOptions) { o.DeterministicSeed = true }
}

// WithNodeHint pre-sizes the pointer-free visited table for graphs with
// approximately n pointer-referenced nodes.
func WithNodeHint(n int) FreezeOption { return func(o *FreezeOptions) { o.NodeHint = n } }

// WithDict enables cross-object string deduplication using d. Strings that
// appear more than once across calls are interned in d instead of being
// copied into each snapshot or Compact value.
func WithDict(d *Dict) FreezeOption { return func(o *FreezeOptions) { o.Dict = d } }

// visitedEntry is one slot of the visited table. It is pointer-free: addr
// is the integer address of a source object that the caller keeps alive.
type visitedEntry struct {
	addr uintptr
	tbl  uint32
	val  uint32
}

const (
	visitInStack = 1
	visitDone    = 2
)

// visitedTable is an open-addressed hash table from (address, table) to a
// 32-bit value. Different tables are distinguished because a struct and its
// first field share an address.
type visitedTable struct {
	slots []visitedEntry
	n     int
}

func (t *visitedTable) find(addr uintptr, tbl uint32) *visitedEntry {
	if len(t.slots) == 0 {
		return nil
	}
	mask := uint64(len(t.slots) - 1)
	i := HashUint64(uint64(addr)^uint64(tbl)<<56, DefaultSeed) & mask
	for {
		e := &t.slots[i]
		if e.addr == 0 {
			return nil
		}
		if e.addr == addr && e.tbl == tbl {
			return e
		}
		i = (i + 1) & mask
	}
}

func (t *visitedTable) insert(addr uintptr, tbl, val uint32) *visitedEntry {
	if 2*(t.n+1) > len(t.slots) {
		t.grow()
	}
	mask := uint64(len(t.slots) - 1)
	i := HashUint64(uint64(addr)^uint64(tbl)<<56, DefaultSeed) & mask
	for {
		e := &t.slots[i]
		if e.addr == 0 {
			*e = visitedEntry{addr: addr, tbl: tbl, val: val}
			t.n++
			return e
		}
		if e.addr == addr && e.tbl == tbl {
			e.val = val
			return e
		}
		i = (i + 1) & mask
	}
}

// minVisitedSlots is the initial size of a visited table. It is small so
// that freezing a single object, as Pack functions do, stays cheap; larger
// graphs double the table as needed.
const minVisitedSlots = 16

func (t *visitedTable) grow() {
	size := minVisitedSlots
	if len(t.slots) > 0 {
		size = 2 * len(t.slots)
	}
	old := t.slots
	t.slots = make([]visitedEntry, size)
	t.n = 0
	for _, e := range old {
		if e.addr != 0 {
			t.insert(e.addr, e.tbl, e.val)
		}
	}
}

func (t *visitedTable) resetValues() {
	for i := range t.slots {
		t.slots[i].val = 0
	}
}

// internSlot is one entry of the pointer-free string interning cache.
// Storing the full 64-bit hash eliminates birthday-paradox false positives
// between the first and second passes on large string sets.
type internSlot struct {
	hash uint64
	off  uint32
	len  uint32
}

// MaxInternSlots is the size of the largest string interning cache
// (1<<20 slots of 16 bytes, 16 MiB).
const MaxInternSlots = 1 << 20

const (
	minInternSlots    = 32
	internSparseSlots = 64
)

// internCache is the bounded, pointer-free string interning cache: a
// two-way set-associative table of (hash, offset, length) slots indexed by
// the low bits of the string's 64-bit hash. It starts with 32 slots and
// doubles in place, up to MaxInternSlots, whenever more than half of its
// slots are filled while it is small (below 64 slots) and more than one
// eighth afterwards; at that load two strings rarely compete for the same
// pair. Beyond 64 slots its storage is allocated in steps of 4x, so that a
// large freeze reallocates it only a few times.
//
// Both passes of a freeze drive an identical cache with the same sequence
// of strings: the first pass with 64-bit hashes and lengths only, the
// second pass also comparing bytes. The first pass therefore predicts
// which strings the second pass deduplicates and sizes the string blob to
// the deduplicated total instead of the sum of all string lengths.
type internCache struct {
	slots  []internSlot
	filled int
}

// reset empties the cache and restarts it at the minimum size inside store,
// whose capacity later growth reuses. Callers pass either smallIntern[:] or
// c.slots, whose tail [len(store):cap(store)] has never been written.
func (c *internCache) reset(store []internSlot) {
	clear(store)
	c.slots = store[:minInternSlots]
	c.filled = 0
}

func (c *internCache) pair(h uint64) (uint64, uint64) {
	i := h & uint64(len(c.slots)-1)
	return i, i ^ 1
}

// seen reports whether a string with hash h and length n is in the cache
// and inserts it if not. It is the first-pass counterpart of lookup and
// insert.
func (c *internCache) seen(h uint64, n uint32) bool {
	i, j := c.pair(h)
	if e := c.slots[i]; e.len == n && e.hash == h {
		return true
	}
	if e := c.slots[j]; e.len == n && e.hash == h {
		return true
	}
	c.insert(internSlot{hash: h, len: n})
	return false
}

// insert stores e in the pair of its hash, preferring the first slot unless
// only the second is free, and grows the cache when its load exceeds the
// limit for its size.
func (c *internCache) insert(e internSlot) {
	i, j := c.pair(e.hash)
	k := i
	if c.slots[i].len != 0 && c.slots[j].len == 0 {
		k = j
	}
	if c.slots[k].len == 0 {
		c.filled++
	}
	c.slots[k] = e
	load := 2
	if len(c.slots) >= internSparseSlots {
		load = 8
	}
	if load*c.filled > len(c.slots) && len(c.slots) < MaxInternSlots {
		c.grow()
	}
}

// grow doubles the table. An entry of pair p either stays in p or moves to
// pair p+n, which only entries of p can reach, so the split needs no
// temporary table and is deterministic.
func (c *internCache) grow() {
	n := len(c.slots)
	if cap(c.slots) >= 2*n {
		c.slots = c.slots[:2*n] // the upper half has never been written
	} else {
		size := 2 * n
		if n >= internSparseSlots {
			size = min(4*n, MaxInternSlots)
		}
		s := make([]internSlot, size)
		copy(s, c.slots)
		c.slots = s[:2*n]
	}
	mask := uint64(2*n - 1)
	for p := range n {
		e := c.slots[p]
		if e.len == 0 {
			continue
		}
		i := e.hash & mask
		if i < uint64(n) {
			continue
		}
		c.slots[p] = internSlot{}
		if c.slots[i].len != 0 {
			i ^= 1
		}
		c.slots[i] = e
	}
}

// Builder holds the state shared by the two passes of a generated Freeze
// function: traversal mode and depth, the visited table, table and pool
// counters, the string blob with its interning cache, and the location and
// zoned address side tables.
type Builder struct {
	Mode FreezeMode
	Opts FreezeOptions
	Seed uint64

	// Locs and Zones collect the side tables referenced by flat times and
	// addresses.
	Locs  LocTable
	Zones ZoneTable

	maxDepth int
	depth    int
	visited  visitedTable

	counts []uint32
	ids    []uint32

	strUpper     uint64
	strCount     uint64
	firstStrHash uint64
	firstStrLen  uint32
	strBuf       []byte
	intern       internCache
	exact        map[string]uint32
	usedDict     bool
	reuse        bool   // StrBlob aliases strReuse instead of sealing a fresh buffer
	strReuse     []byte // buffer for the string blob when reuse is set

	// Initial storage for small graphs, so that freezing a single object
	// allocates little beyond the snapshot itself.
	smallVisited [minVisitedSlots]visitedEntry
	smallIntern  [minInternSlots]internSlot
	smallCounts  [16]uint32
}

// NewBuilder returns a Builder for a schema with ntables node tables.
func NewBuilder(mode FreezeMode, ntables int, opts ...FreezeOption) *Builder {
	b := &Builder{}
	b.Init(mode, ntables, opts...)
	return b
}

// Reset releases heap references held by b so that a pooled Builder does not
// retain snapshots, dictionaries or large temporary tables. The fixed-size
// pointer-free scratch arrays are cleared lazily by Init only when needed.
func (b *Builder) Reset() {
	b.Opts = FreezeOptions{}
	b.Locs = LocTable{}
	b.Zones = ZoneTable{}
	b.visited = visitedTable{}
	b.counts = nil
	b.ids = nil
	b.strBuf = nil
	b.intern = internCache{}
	b.exact = nil
	b.strReuse = nil
}

// Init initializes or resets b in place for a schema with ntables node
// tables. It allows callers such as generated Pack functions to allocate a
// Builder on the stack or reuse one from a pool. If FreezeOptions.Dict is
// closed, Init fails with ErrUnknownDict through Fail, so it must be called
// under Recover like the rest of a Freeze.
func (b *Builder) Init(mode FreezeMode, ntables int, opts ...FreezeOption) {
	b.Mode = mode
	b.Opts = FreezeOptions{}
	b.Locs = LocTable{}
	b.Zones = ZoneTable{}
	b.depth = 0
	b.visited = visitedTable{}
	b.strUpper = 0
	b.strCount = 0
	b.firstStrHash = 0
	b.firstStrLen = 0
	b.strBuf = nil
	b.intern = internCache{}
	b.exact = nil
	b.usedDict = false
	b.reuse = false
	b.strReuse = nil
	if 2*ntables <= len(b.smallCounts) {
		cs := b.smallCounts[:2*ntables]
		clear(cs)
		b.counts, b.ids = cs[:ntables:ntables], cs[ntables:2*ntables:2*ntables]
	} else {
		cs := make([]uint32, 2*ntables)
		b.counts, b.ids = cs[:ntables:ntables], cs[ntables:2*ntables:2*ntables]
	}
	for _, o := range opts {
		o(&b.Opts)
	}
	if mode != ModeTree {
		if b.Opts.NodeHint > 0 {
			hint := min(b.Opts.NodeHint, int(MaxElems/2))
			want := 1 << bits.Len(uint(2*hint-1))
			if want > minVisitedSlots {
				b.visited.slots = make([]visitedEntry, want)
			} else {
				clear(b.smallVisited[:])
				b.visited.slots = b.smallVisited[:]
			}
		} else {
			clear(b.smallVisited[:])
			b.visited.slots = b.smallVisited[:]
		}
	}
	b.maxDepth = min(b.Opts.MaxDepth, MaxGraphDepth)
	if b.maxDepth <= 0 {
		if mode == ModeTree {
			b.maxDepth = DefaultTreeMaxDepth
		} else {
			b.maxDepth = DefaultGraphMaxDepth
		}
	}
	if b.Opts.Dict.Closed() {
		Fail(ErrUnknownDict)
	}
	b.Seed = b.Opts.HashSeed
	if b.Seed == 0 {
		if b.Opts.DeterministicSeed {
			b.Seed = DefaultSeed
		} else {
			b.Seed = RandomSeed()
		}
	}
	b.Locs.PreserveLocal = b.Opts.PreserveLocal
}

// Count returns the number of nodes counted for table tbl in the first
// pass.
func (b *Builder) Count(tbl int) int { return int(b.counts[tbl]) }

func (b *Builder) enter() {
	b.depth++
	if b.depth > b.maxDepth {
		if b.Mode == ModeTree {
			Fail(ErrCycleDetected)
		}
		Fail(ErrCapacityExceeded)
	}
}

func (b *Builder) countNode(tbl int) {
	if b.counts[tbl] >= MaxElems {
		Fail(ErrCapacityExceeded)
	}
	b.counts[tbl]++
}

// MeasureEnter is called by the first pass before visiting the object at
// addr stored in table tbl. It reports whether the object must be visited;
// it returns false for objects that were already visited and fails with
// ErrCycleDetected when ModeDAG reaches an object that is still on the
// traversal stack.
func (b *Builder) MeasureEnter(addr uintptr, tbl int) bool {
	if b.Mode != ModeTree {
		if e := b.visited.find(addr, uint32(tbl)); e != nil {
			if e.val == visitInStack && b.Mode == ModeDAG {
				Fail(ErrCycleDetected)
			}
			return false
		}
		b.visited.insert(addr, uint32(tbl), visitInStack)
	}
	b.enter()
	return true
}

// MeasureLeave is called by the first pass after visiting the object at
// addr entered with MeasureEnter.
func (b *Builder) MeasureLeave(addr uintptr, tbl int) {
	if b.Mode != ModeTree {
		b.visited.insert(addr, uint32(tbl), visitDone)
	}
	b.depth--
	b.countNode(tbl)
}

// MeasureEnterValue is called by the first pass before visiting a struct
// value that is not referenced through a pointer.
func (b *Builder) MeasureEnterValue() { b.enter() }

// MeasureLeaveValue is called by the first pass after visiting a struct
// value stored in table tbl.
func (b *Builder) MeasureLeaveValue(tbl int) {
	b.depth--
	b.countNode(tbl)
}

// MeasureString accounts for a string that the second pass will pass to
// String. With the default interning cache, strings the second pass will
// find in its cache are not counted, so the string blob is allocated at its
// final size.
func (b *Builder) MeasureString(s string) {
	if len(s) == 0 {
		return
	}
	if b.Opts.Dict != nil {
		if b.Opts.Dict.Closed() {
			Fail(ErrUnknownDict)
		}
		if _, ok := b.Opts.Dict.Lookup(s); ok {
			return
		}
	}
	b.measureLocalString(s)
}

func (b *Builder) measureLocalString(s string) {
	b.strCount++
	if !b.Opts.NoStringDedup && !b.Opts.ExactStringDedup {
		if b.intern.slots == nil {
			h := HashString(s, DefaultSeed)
			if b.strCount == 1 {
				b.firstStrHash = h
				b.firstStrLen = uint32(len(s))
				b.strUpper += uint64(len(s))
				if b.strUpper > MaxBlob {
					Fail(ErrCapacityExceeded)
				}
				return
			}
			b.intern.reset(b.smallIntern[:])
			b.intern.insert(internSlot{hash: b.firstStrHash, len: b.firstStrLen})
			if b.intern.seen(h, uint32(len(s))) {
				return
			}
		} else if b.intern.seen(HashString(s, DefaultSeed), uint32(len(s))) {
			return
		}
	}
	b.strUpper += uint64(len(s))
	if b.strUpper > MaxBlob {
		Fail(ErrCapacityExceeded)
	}
}

// MeasureBytes accounts for a byte slice that the second pass will pass to
// Bytes. Byte slices are always stored in the local string blob.
func (b *Builder) MeasureBytes(p []byte) {
	if len(p) > 0 {
		b.measureLocalString(UnsafeSealBytes(p))
	}
}

// GrowPool adds n elements to the pool counter *c, failing with
// ErrCapacityExceeded on overflow.
func GrowPool(c *uint32, n int) {
	if n < 0 || uint64(*c)+uint64(n) > MaxElems {
		Fail(ErrCapacityExceeded)
	}
	*c += uint32(n)
}

// HashRegion returns the size of the hash index region of a hash-indexed
// map with n entries: the smallest power of two that is at least 2n, or 0
// for an empty map.
func HashRegion(n int) int {
	if n == 0 {
		return 0
	}
	return 1 << bits.Len(uint(2*n-1))
}

// BeginEmit prepares the second pass: it allocates the string blob with
// the size measured by the first pass, restarts the interning cache in the
// storage the first pass grew it to, and resets the visited table values.
func (b *Builder) BeginEmit() {
	if b.reuse && cap(b.strReuse) >= int(b.strUpper) {
		b.strBuf = b.strReuse[:0]
	} else {
		b.strBuf = make([]byte, 0, int(b.strUpper))
	}
	switch {
	case b.Opts.NoStringDedup || b.strCount <= 1:
		b.intern.slots = nil
	case b.Opts.ExactStringDedup:
		b.exact = make(map[string]uint32, int(min(b.strCount, 1<<20)))
	default:
		b.intern.reset(b.intern.slots)
	}
	if b.Mode != ModeTree {
		b.visited.resetValues()
	}
	clear(b.ids)
}

// Lookup returns the ID assigned in the second pass to the object at addr
// in table tbl, or 0 if it has not been assigned yet.
func (b *Builder) Lookup(addr uintptr, tbl int) uint32 {
	if e := b.visited.find(addr, uint32(tbl)); e != nil {
		return e.val
	}
	return 0
}

// Record stores the ID of the object at addr in table tbl.
func (b *Builder) Record(addr uintptr, tbl int, id uint32) {
	b.visited.insert(addr, uint32(tbl), id)
}

// PreID assigns the next ID of table tbl in pre-order (ModeCyclic).
func (b *Builder) PreID(tbl int) uint32 {
	if b.ids[tbl] >= b.counts[tbl] {
		Fail(ErrConcurrentModification)
	}
	b.ids[tbl]++
	return b.ids[tbl]
}

// PostID assigns the next ID of table tbl in reverse post-order (ModeTree
// and ModeDAG), so that children receive larger IDs than their parents.
func (b *Builder) PostID(tbl int) uint32 {
	if b.ids[tbl] >= b.counts[tbl] {
		Fail(ErrConcurrentModification)
	}
	id := b.counts[tbl] - b.ids[tbl]
	b.ids[tbl]++
	return id
}

// Assigned returns the number of IDs assigned to table tbl in the second
// pass.
func (b *Builder) Assigned(tbl int) int { return int(b.ids[tbl]) }

// Preorder reports whether IDs are assigned in pre-order.
func (b *Builder) Preorder() bool { return b.Mode == ModeCyclic }

// String appends s to the string blob (or finds an interned copy) and
// returns its span. When FreezeOptions.Dict is configured, repeated strings
// may be encoded as dictionary spans with DictSpanBit set.
func (b *Builder) String(s string) Span32 {
	if len(s) == 0 {
		return Span32{}
	}
	if b.Opts.Dict != nil {
		if b.Opts.Dict.Closed() {
			Fail(ErrUnknownDict)
		}
		if sp, ok := b.Opts.Dict.Intern(s); ok {
			b.usedDict = true
			return sp
		}
		if b.Opts.Dict.Closed() {
			Fail(ErrUnknownDict)
		}
	}
	return b.localString(s)
}

func (b *Builder) localString(s string) Span32 {
	switch {
	case b.intern.slots != nil:
		h := HashString(s, DefaultSeed)
		i, j := b.intern.pair(h)
		for _, k := range [2]uint64{i, j} {
			sl := &b.intern.slots[k]
			if sl.len == uint32(len(s)) && sl.hash == h && string(b.strBuf[sl.off:sl.off+sl.len]) == s {
				return Span32{Off: sl.off, Len: sl.len}
			}
		}
		sp := b.appendString(s)
		b.intern.insert(internSlot{hash: h, off: sp.Off, len: sp.Len})
		return sp
	case b.exact != nil:
		if off, ok := b.exact[s]; ok {
			return Span32{Off: off, Len: uint32(len(s))}
		}
		sp := b.appendString(s)
		// The written prefix of strBuf is never modified, so the key may
		// alias it.
		b.exact[UnsafeSealBytes(b.strBuf[sp.Off:sp.End()])] = sp.Off
		return sp
	}
	return b.appendString(s)
}

// UsedDict reports whether any string in the second pass was encoded as a
// dictionary span.
func (b *Builder) UsedDict() bool { return b.usedDict }

func (b *Builder) appendString(s string) Span32 {
	if len(b.strBuf)+len(s) > cap(b.strBuf) {
		// Without the interning cache the first pass measured an exact
		// upper bound, so exceeding it means that the source graph was
		// mutated concurrently with Freeze. With the cache the prediction
		// can also miss if two distinct strings share a 64-bit hash and
		// length; the buffer then grows.
		if b.intern.slots == nil || uint64(len(b.strBuf))+uint64(len(s)) > MaxBlob {
			Fail(ErrConcurrentModification)
		}
	}
	off := len(b.strBuf)
	b.strBuf = append(b.strBuf, s...)
	return Span32{Off: uint32(off), Len: uint32(len(s))}
}

// Bytes appends p to the local string blob and returns its span; a nil p is
// encoded as NilSpan.
func (b *Builder) Bytes(p []byte) Span32 {
	if p == nil {
		return NilSpan
	}
	if len(p) == 0 {
		return Span32{}
	}
	return b.localString(UnsafeSealBytes(p))
}

// Time encodes t, registering its location in the location table.
func (b *Builder) Time(t time.Time) FlatTime { return b.Locs.Encode(t) }

// StrBlob seals and returns the string blob. The blob is sealed without a
// copy unless more than 1/8 of its capacity (and at least 4 KiB) is unused,
// in which case a right-sized copy is returned, so that a snapshot never
// retains more than 12.5% of slack. With the default interning cache the
// first pass predicts deduplication and the blob is normally full; slack
// arises with ExactStringDedup, whose first pass counts every string.
//
// StrBlob must be called after the last ID has been assigned: it releases
// the visited table and the interning cache first, so that a copy does not
// coexist with them.
func (b *Builder) StrBlob() string {
	if b.usedDict && b.Opts.Dict.Closed() {
		Fail(ErrUnknownDict)
	}
	buf := b.strBuf
	b.strBuf = nil
	b.intern = internCache{}
	b.exact = nil
	b.visited = visitedTable{}
	if b.reuse {
		b.strReuse = buf[:0]
		return UnsafeSealBytes(buf)
	}
	if slack := cap(buf) - len(buf); slack >= 4<<10 && slack > cap(buf)/8 {
		return string(buf)
	}
	return UnsafeSealBytes(buf[:len(buf):len(buf)])
}

// ReuseStrBuf makes the second pass build the string blob in buf when its
// capacity suffices, instead of allocating. StrBlob then returns a string
// that aliases the buffer, and StrBuf returns the buffer for reuse, so the
// blob must be consumed before the buffer is reused. ReuseStrBuf must be
// called after Init and before BeginEmit.
func (b *Builder) ReuseStrBuf(buf []byte) {
	b.reuse = true
	b.strReuse = buf
}

// StrBuf returns the string blob buffer retained by ReuseStrBuf mode after
// StrBlob, or nil.
func (b *Builder) StrBuf() []byte { return b.strReuse }

// StrLen returns the current length of the string blob.
func (b *Builder) StrLen() int { return len(b.strBuf) }
