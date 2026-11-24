package cnfrt

import (
	"math/bits"
	"sync"
	"sync/atomic"
)

// DefaultDictMaxBytes is the default memory budget of a Dict (32 MiB).
const DefaultDictMaxBytes = 32 << 20

// MinDictMaxBytes is the smallest memory budget accepted by NewDict; smaller
// positive budgets are raised to it.
const MinDictMaxBytes = 4 << 10

// DictSpanBit is the high bit of Span32.Off marking a string stored in a
// shared Dict rather than the snapshot's local string blob. Because empty
// strings never use DictSpanBit and NilSpan has Len == 0, DictSpanBit with
// Len > 0 never collides with NilSpan.
const DictSpanBit uint32 = 1 << 31

const (
	dictChunkShift  = 21 // 2 MiB logical chunk stride
	dictChunkMax    = 1 << dictChunkShift
	dictMaxBytesCap = 1 << 31 // 2 GiB (31-bit offset space)
	dictMinSeen     = 64
	dictMaxSeen     = 1 << 18 // 1 MiB seen-twice filter (262,144 uint32 slots)
	dictMinSlots    = 8
	dictMaxInitSlot = 256
	dictMaxStrLen   = 65536
	dictSlotBytes   = 16 // size of dictSlot
	dictSeenBytes   = 4  // size of one seen-twice filter entry
	dictMaxMisses   = 8  // consecutive budget rejections before a nearly full Dict seals
	dictMaxSlotID   = 1<<16 - 1
	dictNonceMask   = 1<<48 - 1
)

// dictRegistry maps the 16-bit slot of a dictionary token to the open Dict
// holding it. Slots are recycled through dictFree; the 48-bit nonce in the
// token distinguishes successive owners of a slot.
var (
	dictRegistry [1 << 16]atomic.Pointer[Dict]
	dictFree     struct {
		mu   sync.Mutex
		ids  []uint16
		next uint32
	}
)

func acquireDictSlot() uint16 {
	dictFree.mu.Lock()
	defer dictFree.mu.Unlock()
	if n := len(dictFree.ids); n > 0 {
		id := dictFree.ids[n-1]
		dictFree.ids = dictFree.ids[:n-1]
		return id
	}
	if dictFree.next >= dictMaxSlotID {
		return 0
	}
	dictFree.next++
	return uint16(dictFree.next)
}

func releaseDictSlot(id uint16) {
	dictFree.mu.Lock()
	dictFree.ids = append(dictFree.ids, id)
	dictFree.mu.Unlock()
}

// LookupDictToken returns the open Dict whose token is token, or nil if no
// open Dict in this process has that token. It is lock-free.
func LookupDictToken(token uint64) *Dict {
	id := uint16(token)
	if id == 0 {
		return nil
	}
	d := dictRegistry[id].Load()
	if d == nil || d.closed.Load() || d.token != token {
		return nil
	}
	return d
}

// ResolveDictSpan returns the string referenced by (off, n) in the Dict
// whose 8-byte token is stored at raw[dictOff:dictOff+8]. Generated Compact
// string getters call it when DictSpanBit is set. It panics if raw does not
// carry SlimFlagDict, and panics with ErrUnknownDict if the Dict has been
// closed or does not belong to this process.
//
//go:noinline
func ResolveDictSpan(raw string, dictOff int, off, n uint32) string {
	if len(raw) < dictOff+8 || raw[0]&SlimFlagDict == 0 {
		panic("cnfrt: compact value has no dictionary")
	}
	token := r8s(raw[dictOff : dictOff+8])
	id := uint16(token)
	if id == 0 {
		panic(ErrUnknownDict)
	}
	d := dictRegistry[id].Load()
	if d == nil || d.closed.Load() || d.token != token {
		panic(ErrUnknownDict)
	}
	if off&DictSpanBit == 0 || n == 0 {
		panic("cnfrt: corrupt dictionary string span")
	}
	rawOff := off &^ DictSpanBit
	ci := rawOff >> dictChunkShift
	co := rawOff & (dictChunkMax - 1)
	if d.full.Load() {
		strs := *d.strs.Load()
		if int(ci) >= len(strs) {
			panic("cnfrt: corrupt dictionary string span")
		}
		s := strs[ci]
		if uint64(co)+uint64(n) > uint64(len(s)) {
			panic("cnfrt: corrupt dictionary string span")
		}
		return s[co : co+n]
	}
	chunks := *d.chunks.Load()
	if int(ci) >= len(chunks) {
		panic("cnfrt: corrupt dictionary string span")
	}
	ch := chunks[ci]
	if uint64(co)+uint64(n) > uint64(ch.used.Load()) {
		panic("cnfrt: corrupt dictionary string span")
	}
	return ch.str[co : co+n]
}

type dictSlot struct {
	hash uint64
	off  uint32
	len  uint32
}

type dictChunk struct {
	str  string
	buf  []byte // cleared once the chunk is sealed
	used atomic.Uint32
}

// DictStats describes the current contents of a Dict.
type DictStats struct {
	// Entries is the number of interned strings.
	Entries int
	// Bytes is the total length of the interned strings.
	Bytes uint64
	// MemBytes is the memory charged against the Dict's budget: allocated
	// string chunks, the hash index and the seen-twice filter.
	MemBytes uint64
	// Chunks is the number of string chunks.
	Chunks int
	// Full reports whether the Dict has stopped admitting strings.
	Full bool
}

// Dict is a bounded, thread-safe, pointer-free string dictionary shared across
// independent Compact values and snapshots in the same process.
//
// Strings are stored in append-only immutable byte chunks (at most 2 MiB each)
// and indexed by a pointer-free open-addressed hash table (PtrBytes == 0)
// hashed with a per-Dict random seed. A seen-twice admission filter, sized
// to at most 1/8 of the budget, keeps one-off strings (such as UIDs or
// SKUs) out of the dictionary: a string is admitted only on its second
// observation. The memory budget
// covers the string chunks, the hash index and the filter; once it is
// exhausted, the dictionary becomes read-only and serves both lookups and
// reads without locking.
//
// Every Dict has a 64-bit token: the low 16 bits select a process-wide
// registry slot and the high 48 bits are a random nonce. Compact values
// store the token, so a value can only resolve strings from the exact Dict
// instance that encoded it, even after the slot is reused by another Dict.
type Dict struct {
	token    uint64
	seed     uint64
	maxBytes uint64
	chunkCap uint32
	closed   atomic.Bool

	seen     []atomic.Uint32 // pointer-free seen-twice filter
	seenMask uint64

	chunks atomic.Pointer[[]*dictChunk]
	strs   atomic.Pointer[[]string] // chunk strings, parallel to chunks
	full   atomic.Bool

	mu        sync.RWMutex
	slots     []dictSlot // pointer-free open-addressed hash table
	count     int
	bytes     uint64 // total length of interned strings
	alloc     uint64 // total capacity of allocated chunks
	nextChunk uint32 // target size of the next allocated chunk
	misses    int    // consecutive budget rejections
}

// NewDict creates and registers a shared string dictionary whose memory
// (string chunks, hash index and seen-twice filter) is capped at maxBytes.
// If maxBytes <= 0, DefaultDictMaxBytes (32 MiB) is used; budgets below
// MinDictMaxBytes are raised to it.
//
// Call Close when the dictionary and all values referencing it are no longer
// needed, to release its registry slot for reuse. If more than 65,535
// dictionaries are open at once, NewDict returns a Dict that never admits
// strings (its Token is 0), so callers transparently fall back to local
// string storage.
func NewDict(maxBytes int) *Dict {
	limit := uint64(maxBytes)
	switch {
	case maxBytes <= 0:
		limit = DefaultDictMaxBytes
	case limit < MinDictMaxBytes:
		limit = MinDictMaxBytes
	case limit > dictMaxBytesCap:
		limit = dictMaxBytesCap
	}
	// The filter and the initial index together take at most 1/8 of the
	// budget, leaving the rest for strings and index growth.
	seenLen := min(1<<(bits.Len64(limit/(16*dictSeenBytes))-1), dictMaxSeen)
	seenLen = max(seenLen, dictMinSeen)
	slotLen := min(1<<(bits.Len64(limit/(16*dictSlotBytes))-1), dictMaxInitSlot)
	slotLen = max(slotLen, dictMinSlots)
	d := &Dict{
		seed:      RandomSeed(),
		maxBytes:  limit,
		chunkCap:  uint32(min(max(limit/4, MinDictMaxBytes), dictChunkMax)),
		nextChunk: uint32(min(max(limit/16, MinDictMaxBytes), 64<<10)),
		seen:      make([]atomic.Uint32, seenLen),
		seenMask:  uint64(seenLen - 1),
		slots:     make([]dictSlot, slotLen),
	}
	empty := make([]*dictChunk, 0, 16)
	d.chunks.Store(&empty)
	noStrs := []string{}
	d.strs.Store(&noStrs)

	id := acquireDictSlot()
	if id == 0 {
		d.full.Store(true)
		return d
	}
	nonce := RandomSeed() & dictNonceMask
	if nonce == 0 {
		nonce = 1
	}
	d.token = nonce<<16 | uint64(id)
	dictRegistry[id].Store(d)
	return d
}

// Token returns the 64-bit identity of d stored in Slim Compact headers, or
// 0 if d could not be registered.
func (d *Dict) Token() uint64 {
	if d == nil {
		return 0
	}
	return d.token
}

// Closed reports whether Close has been called on d.
func (d *Dict) Closed() bool { return d != nil && d.closed.Load() }

// Close unregisters d and returns its registry slot for reuse. It is
// idempotent. After Close, d admits no strings, Pack and Freeze with d fail
// with ErrUnknownDict, and values referencing d can no longer be resolved:
// checked entry points return ErrUnknownDict, SlabMap.Get and FrozenMap.Get
// fail to decode and return (zero, false), and direct getters panic. Do not
// close d while containers holding Compact values that reference it remain in
// use. Snapshots and views created before Close keep the chunks they captured
// and remain readable; the chunks are freed once they become unreachable.
func (d *Dict) Close() {
	if d == nil || !d.closed.CompareAndSwap(false, true) {
		return
	}
	if id := uint16(d.token); id != 0 && dictRegistry[id].CompareAndSwap(d, nil) {
		releaseDictSlot(id)
	}
}

// overheadLocked returns the memory taken by the hash index and the filter.
func (d *Dict) overheadLocked() uint64 {
	return uint64(len(d.slots))*dictSlotBytes + uint64(len(d.seen))*dictSeenBytes
}

// Stats returns a snapshot of d's occupancy.
func (d *Dict) Stats() DictStats {
	d.mu.RLock()
	st := DictStats{
		Entries:  d.count,
		Bytes:    d.bytes,
		MemBytes: d.alloc + d.overheadLocked(),
		Chunks:   len(*d.chunks.Load()),
		Full:     d.full.Load(),
	}
	d.mu.RUnlock()
	return st
}

// ValidSpan reports whether d is open and sp is a valid dictionary span
// pointing within the committed bytes of d. It is lock-free.
func (d *Dict) ValidSpan(sp Span32) bool {
	_, ok := d.Get(sp)
	return ok
}

// ValidCapturedSpan reports whether d is open, sp is a valid dictionary span
// pointing within the committed bytes of d, and sp addresses a chunk present
// in captured (a slice previously returned by Chunks). Untrusted validators
// call it so chunks added concurrently after Chunks was captured cannot pass
// validation and later index out of bounds. It is lock-free.
func (d *Dict) ValidCapturedSpan(captured []string, sp Span32) bool {
	if d == nil || sp.Off&DictSpanBit == 0 || sp.Len == 0 || d.closed.Load() {
		return false
	}
	rawOff := sp.Off &^ DictSpanBit
	ci := int(rawOff >> dictChunkShift)
	co := rawOff & (dictChunkMax - 1)
	if ci >= len(captured) || uint64(co)+uint64(sp.Len) > uint64(len(captured[ci])) {
		return false
	}
	chunks := *d.chunks.Load()
	if ci >= len(chunks) || uint64(co)+uint64(sp.Len) > uint64(chunks[ci].used.Load()) {
		return false
	}
	return true
}

// Get returns the string referenced by sp, or ("", false) if d is closed or
// sp is invalid. It is lock-free and does not allocate.
func (d *Dict) Get(sp Span32) (string, bool) {
	if d == nil || sp.Off&DictSpanBit == 0 || sp.Len == 0 || d.closed.Load() {
		return "", false
	}
	rawOff := sp.Off &^ DictSpanBit
	ci := rawOff >> dictChunkShift
	co := rawOff & (dictChunkMax - 1)
	chunks := *d.chunks.Load()
	if int(ci) >= len(chunks) {
		return "", false
	}
	ch := chunks[ci]
	if uint64(co)+uint64(sp.Len) > uint64(ch.used.Load()) {
		return "", false
	}
	return ch.str[co : co+sp.Len], true
}

// MustGet returns the string referenced by sp. It panics with
// ErrUnknownDict if d is closed and with a corruption message if sp is out
// of bounds. It is lock-free and does not allocate.
//
//go:noinline
func (d *Dict) MustGet(sp Span32) string {
	s, ok := d.Get(sp)
	if !ok {
		if d.Closed() {
			panic(ErrUnknownDict)
		}
		panic("cnfrt: corrupt dictionary string span")
	}
	return s
}

// Chunks returns the string chunks of d in index order. A dictionary span
// with offset off (DictSpanBit cleared) addresses chunk off>>21 at byte
// off&(1<<21-1). Generated snapshots capture the chunks when they are built
// or loaded, so that their getters resolve validated dictionary spans with
// plain slice operations; chunks are append-only and immutable below their
// committed length, so a captured slice stays valid for every span that
// existed when it was captured.
func (d *Dict) Chunks() []string {
	if d == nil {
		return nil
	}
	return *d.strs.Load()
}

func (d *Dict) lookupLocked(h uint64, s string) (Span32, bool) {
	mask := uint64(len(d.slots) - 1)
	chunks := *d.chunks.Load()
	for i := h & mask; ; i = (i + 1) & mask {
		sl := d.slots[i]
		if sl.len == 0 {
			return Span32{}, false
		}
		if sl.hash == h && int(sl.len) == len(s) {
			ci := sl.off >> dictChunkShift
			co := sl.off & (dictChunkMax - 1)
			if chunks[ci].str[co:co+sl.len] == s {
				return Span32{Off: DictSpanBit | sl.off, Len: sl.len}, true
			}
		}
	}
}

// Lookup looks up s in d without updating the seen-twice filter or admitting
// new strings. It is safe for concurrent use and lock-free once d is full.
func (d *Dict) Lookup(s string) (Span32, bool) {
	if d == nil || len(s) == 0 || len(s) > dictMaxStrLen || d.token == 0 || d.closed.Load() {
		return Span32{}, false
	}
	h := HashString(s, d.seed)
	if d.full.Load() {
		return d.lookupLocked(h, s)
	}
	d.mu.RLock()
	sp, ok := d.lookupLocked(h, s)
	d.mu.RUnlock()
	return sp, ok
}

// Intern returns a dictionary Span32 for s if s is already in d or if s has
// been observed at least once before and fits within the memory budget. On a
// string's first observation Intern records its fingerprint in the
// seen-twice filter without taking a write lock and returns (Span32{}, false).
func (d *Dict) Intern(s string) (Span32, bool) {
	if d == nil || len(s) == 0 || len(s) > dictMaxStrLen || d.token == 0 || d.closed.Load() {
		return Span32{}, false
	}
	h := HashString(s, d.seed)
	if d.full.Load() {
		return d.lookupLocked(h, s)
	}
	d.mu.RLock()
	if sp, ok := d.lookupLocked(h, s); ok {
		d.mu.RUnlock()
		return sp, true
	}
	d.mu.RUnlock()

	// Seen-twice admission filter: record hash on first sighting, admit on
	// second sighting.
	idx := h & d.seenMask
	tag := uint32(h>>32) | 1
	if d.seen[idx].Load() != tag {
		d.seen[idx].Store(tag)
		return Span32{}, false
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if sp, ok := d.lookupLocked(h, s); ok {
		return sp, true
	}
	if d.full.Load() {
		return Span32{}, false
	}
	if !d.reserveLocked(uint64(len(s))) {
		d.rejectLocked()
		return Span32{}, false
	}
	off := d.appendLocked(s)
	d.insertLocked(dictSlot{hash: h, off: off, len: uint32(len(s))})
	d.bytes += uint64(len(s))
	d.misses = 0
	if d.maxBytes-d.alloc-d.overheadLocked() < 64 && d.lastChunkFree() < 64 {
		d.sealLocked()
	}
	return Span32{Off: DictSpanBit | off, Len: uint32(len(s))}, true
}

// sealLocked publishes chunk strings trimmed to their committed lengths,
// clears writable chunk slice headers, and marks d full so lock-free reads
// in ResolveDictSpan cannot observe unwritten tail bytes. Caller holds d.mu.
func (d *Dict) sealLocked() {
	chunks := *d.chunks.Load()
	strs := make([]string, len(chunks))
	for i, ch := range chunks {
		ch.buf = nil
		strs[i] = ch.str[:ch.used.Load()]
	}
	d.strs.Store(&strs)
	d.full.Store(true)
}

// lastChunkFree returns the number of unused bytes in the writable chunk.
func (d *Dict) lastChunkFree() uint64 {
	chunks := *d.chunks.Load()
	if len(chunks) == 0 {
		return 0
	}
	last := chunks[len(chunks)-1]
	if last.buf == nil {
		return 0
	}
	return uint64(len(last.buf)) - uint64(last.used.Load())
}

// reserveLocked makes room for one more entry of n bytes: it grows the hash
// index and allocates a new chunk as needed, and reports false without
// changing d if the memory budget does not allow it. The index normally
// doubles at load 1/2; when doubling does not fit the budget it fills up to
// load 3/4, after which d is sealed because no further entry can be indexed.
func (d *Dict) reserveLocked(n uint64) bool {
	used := d.alloc + d.overheadLocked()
	growIndex := 2*(d.count+1) > len(d.slots)
	if growIndex && used+uint64(len(d.slots))*dictSlotBytes > d.maxBytes {
		if 4*(d.count+1) > 3*len(d.slots) {
			d.sealLocked()
			return false
		}
		growIndex = false
	}
	if growIndex {
		used += uint64(len(d.slots)) * dictSlotBytes
	}
	needChunk := d.lastChunkFree() < n
	if needChunk {
		if used >= d.maxBytes {
			return false
		}
		rem := d.maxBytes - used
		chunks := *d.chunks.Load()
		if rem < n || len(chunks) >= dictMaxBytesCap>>dictChunkShift {
			return false
		}
		var size uint64
		if rem < uint64(d.nextChunk) {
			size = min(max(n, rem/2), dictChunkMax)
		} else {
			size = min(max(n, uint64(d.nextChunk)), dictChunkMax)
		}
		if len(chunks) > 0 {
			// Seal the previous chunk's writable slice header.
			chunks[len(chunks)-1].buf = nil
		}
		buf := make([]byte, size)
		str := UnsafeSealBytes(buf)
		next := make([]*dictChunk, len(chunks)+1)
		copy(next, chunks)
		next[len(chunks)] = &dictChunk{str: str, buf: buf}
		strs := append(append(make([]string, 0, len(next)), *d.strs.Load()...), str)
		d.strs.Store(&strs)
		d.chunks.Store(&next)
		d.alloc += size
		if d.nextChunk < d.chunkCap {
			if d.nextChunk > d.chunkCap/2 {
				d.nextChunk = d.chunkCap
			} else {
				d.nextChunk *= 2
			}
		}
	}
	if growIndex {
		d.growLocked()
	}
	return true
}

// rejectLocked records a budget rejection and seals d once it can no longer
// usefully admit strings: when less than 64 bytes of budget remain, or
// after dictMaxMisses consecutive rejections with at least 75% of the
// budget in use.
func (d *Dict) rejectLocked() {
	d.misses++
	used := d.alloc + d.overheadLocked()
	if used > d.maxBytes || d.maxBytes-used < 64 && d.lastChunkFree() < 64 ||
		d.misses >= dictMaxMisses && 4*used >= 3*d.maxBytes {
		d.sealLocked()
	}
}

func (d *Dict) appendLocked(s string) uint32 {
	chunks := *d.chunks.Load()
	ci := uint32(len(chunks) - 1)
	ch := chunks[ci]
	pos := ch.used.Load()
	copy(ch.buf[pos:pos+uint32(len(s))], s)
	ch.used.Store(pos + uint32(len(s)))
	return ci<<dictChunkShift | pos
}

func (d *Dict) insertLocked(e dictSlot) {
	mask := uint64(len(d.slots) - 1)
	for i := e.hash & mask; ; i = (i + 1) & mask {
		if d.slots[i].len == 0 {
			d.slots[i] = e
			d.count++
			return
		}
	}
}

func (d *Dict) growLocked() {
	old := d.slots
	d.slots = make([]dictSlot, 2*len(old))
	mask := uint64(len(d.slots) - 1)
	for _, e := range old {
		if e.len == 0 {
			continue
		}
		for i := e.hash & mask; ; i = (i + 1) & mask {
			if d.slots[i].len == 0 {
				d.slots[i] = e
				break
			}
		}
	}
}
