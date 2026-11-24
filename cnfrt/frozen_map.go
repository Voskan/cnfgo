package cnfrt

import (
	"encoding/binary"
	"fmt"
	"iter"
	"math/bits"
	"runtime"
	"runtime/debug"
	"strings"
)

// FrozenSlot is one slot of a FrozenMap: the key and value spans inside the
// map's data blob.
type FrozenSlot struct {
	KeyOff uint32
	KeyLen uint32
	ValOff uint32
	ValLen uint32
}

const (
	groupSlots = 8
	ctrlEmpty  = 0x80
	lsbs       = 0x0101010101010101
	msbs       = 0x8080808080808080
)

// FrozenMap is an immutable hash table whose storage is pointer-free: a
// control byte array, a slot array of spans and one data blob holding the
// canonical key encodings and value encodings. Lookups hash keys with the
// deterministic wyhash function under the map's seed, so a FrozenMap can be
// persisted to a .cnf file and probed by other processes without being
// rebuilt.
//
// Float keys are canonicalized: NaN keys are rejected when the map is built
// and never found by Get, and -0.0 and +0.0 are the same key. Float values
// keep their exact bits.
//
// A FrozenMap is safe for concurrent use.
type FrozenMap[K comparable, V any] struct {
	seed      uint64
	ctrl      []byte
	slots     []FrozenSlot
	data      string
	numGroups uint32
	n         int
	kc        *keyCodec[K]
	vc        Codec[V]
}

// MapOptions configures FrozenMap and SlabMap construction.
type MapOptions[V any] struct {
	// Seed is the hash seed; zero selects a random seed unless
	// DeterministicSeed is true, in which case DefaultSeed is used. A fixed
	// seed disables reseeding, so it is meant for trusted keys: keys crafted
	// to collide under a known seed degrade the build to quadratic probing,
	// and Validate (VerifyUntrusted) rejects the resulting table once its
	// linear probe budget is exceeded.
	Seed uint64
	// DeterministicSeed selects DefaultSeed when Seed is zero, producing
	// reproducible map layouts across runs. The caveats of a fixed Seed
	// apply.
	DeterministicSeed bool
	// Value is the value codec; nil selects DefaultCodec.
	Value Codec[V]
	// CloneStrings makes decoded keys and values independent of the map's
	// storage: every decoded string, byte slice and self-encoded value
	// (such as a generated Compact value) is a copy. unsafemmap.OpenFrozenMap
	// always sets it for maps backed by memory-mapped files, and SlabMap
	// always behaves as if it were set.
	CloneStrings bool
}

func resolveOptions[K comparable, V any](o *MapOptions[V]) (uint64, *keyCodec[K], Codec[V], error) {
	var opts MapOptions[V]
	if o != nil {
		opts = *o
	}
	if opts.Seed == 0 {
		if opts.DeterministicSeed {
			opts.Seed = DefaultSeed
		} else {
			opts.Seed = RandomSeed()
		}
	}
	kc, err := newKeyCodec[K]()
	if err != nil {
		return 0, nil, nil, err
	}
	kc.clone = opts.CloneStrings
	vc := opts.Value
	if vc == nil {
		vc, err = DefaultCodec[V]()
		if err != nil {
			return 0, nil, nil, err
		}
	}
	if opts.CloneStrings {
		vc = withClone(vc)
	}
	return opts.Seed, kc, vc, nil
}

// FrozenBuilder accumulates entries for a FrozenMap.
type FrozenBuilder[K comparable, V any] struct {
	seed      uint64
	fixedSeed bool
	kc        *keyCodec[K]
	vc        Codec[V]
	valign    int
	data      []byte
	slots     []FrozenSlot
	hashes    []uint64
	err       error
}

// NewFrozenBuilder returns a builder for a FrozenMap with about sizeHint
// entries.
func NewFrozenBuilder[K comparable, V any](sizeHint int, o *MapOptions[V]) (*FrozenBuilder[K, V], error) {
	seed, kc, vc, err := resolveOptions[K, V](o)
	if err != nil {
		return nil, err
	}
	fixed := o != nil && (o.Seed != 0 || o.DeterministicSeed)
	return &FrozenBuilder[K, V]{
		seed:      seed,
		fixedSeed: fixed,
		kc:        kc,
		vc:        vc,
		valign:    valueAlign(vc),
		slots:     make([]FrozenSlot, 0, sizeHint),
		hashes:    make([]uint64, 0, sizeHint),
	}, nil
}

// GrowData ensures the builder's data buffer has capacity for at least n
// additional bytes.
func (b *FrozenBuilder[K, V]) GrowData(n int) {
	if n <= 0 || cap(b.data)-len(b.data) >= n {
		return
	}
	buf := make([]byte, len(b.data), len(b.data)+n)
	copy(buf, b.data)
	b.data = buf
}

// Add adds an entry. If the same key is added more than once, the last
// value wins. It returns ErrNaNKey for keys containing NaN, or any error
// returned by a FallibleCodec.
func (b *FrozenBuilder[K, V]) Add(k K, v V) error {
	if b.err != nil {
		return b.err
	}
	koff := len(b.data)
	var err error
	if b.kc.isString {
		b.data = append(b.data, b.kc.str(&k)...)
	} else {
		b.data, err = b.kc.append(b.data, &k)
		if err != nil {
			b.data = b.data[:koff]
			return err
		}
	}
	klen := len(b.data) - koff
	b.pad()
	voff := len(b.data)
	b.data, err = AppendValue(b.vc, b.data, v)
	if err != nil {
		b.data = b.data[:koff]
		return err
	}
	return b.push(koff, klen, voff, len(b.data)-voff)
}

// AddEncoded adds an entry whose value is already encoded with the map's
// value codec.
func (b *FrozenBuilder[K, V]) AddEncoded(k K, val string) error {
	if b.err != nil {
		return b.err
	}
	koff := len(b.data)
	var err error
	if b.kc.isString {
		b.data = append(b.data, b.kc.str(&k)...)
	} else {
		b.data, err = b.kc.append(b.data, &k)
		if err != nil {
			b.data = b.data[:koff]
			return err
		}
	}
	klen := len(b.data) - koff
	b.pad()
	voff := len(b.data)
	b.data = append(b.data, val...)
	return b.push(koff, klen, voff, len(val))
}

// AddRaw adds an entry from its canonical key encoding and value encoding,
// as returned by FrozenMap.RawEntries.
func (b *FrozenBuilder[K, V]) AddRaw(key, val string) error {
	if b.err != nil {
		return b.err
	}
	koff := len(b.data)
	b.data = append(b.data, key...)
	b.pad()
	voff := len(b.data)
	b.data = append(b.data, val...)
	return b.push(koff, len(key), voff, len(val))
}

// pad appends zero bytes up to the value alignment of the codec.
func (b *FrozenBuilder[K, V]) pad() {
	for len(b.data)&(b.valign-1) != 0 {
		b.data = append(b.data, 0)
	}
}

func (b *FrozenBuilder[K, V]) push(koff, klen, voff, vlen int) error {
	if uint64(len(b.data)) > MaxElems || uint64(len(b.slots)) >= MaxElems/2 {
		b.err = ErrCapacityExceeded
		return b.err
	}
	b.slots = append(b.slots, FrozenSlot{KeyOff: uint32(koff), KeyLen: uint32(klen), ValOff: uint32(voff), ValLen: uint32(vlen)})
	b.hashes = append(b.hashes, HashString(UnsafeSealBytes(b.data[koff:koff+klen]), b.seed))
	return nil
}

// Len returns the number of entries added so far, including duplicates.
func (b *FrozenBuilder[K, V]) Len() int { return len(b.slots) }

// Finish builds the map. The builder must not be used afterwards.
func (b *FrozenBuilder[K, V]) Finish() (*FrozenMap[K, V], error) {
	if b.err != nil {
		return nil, b.err
	}
	n := len(b.slots)
	groups := 1
	for groups*7 < n {
		groups <<= 1
	}
	m := &FrozenMap[K, V]{
		seed:      b.seed,
		ctrl:      make([]byte, groups*groupSlots),
		slots:     make([]FrozenSlot, groups*groupSlots),
		numGroups: uint32(groups),
		kc:        b.kc,
		vc:        b.vc,
	}
	data := UnsafeSealBytes(b.data)
	const maxReseeds = 4
	for attempt := 0; ; attempt++ {
		for i := range m.ctrl {
			m.ctrl[i] = ctrlEmpty
		}
		clear(m.slots)
		m.n = 0
		// The probe budget matches Validate's: a table that exceeds it is
		// abandoned as soon as it does, so a bad seed costs O(N) before
		// reseeding rather than a full quadratic pass.
		reseed := !b.fixedSeed && attempt < maxReseeds
		budget := uint64(8*n + 64)
		var totalProbes uint64
		exceeded := false
		for i, s := range b.slots {
			h := b.hashes[i]
			if attempt > 0 {
				kend := uint64(s.KeyOff) + uint64(s.KeyLen)
				h = HashString(data[uint64(s.KeyOff):kend], m.seed)
			}
			totalProbes += m.insert(data, s, h)
			if reseed && totalProbes > budget {
				exceeded = true
				break
			}
		}
		if !exceeded {
			break
		}
		m.seed = RandomSeed()
	}
	m.data = data
	b.data, b.slots, b.hashes = nil, nil, nil
	return m, nil
}

func (m *FrozenMap[K, V]) insert(data string, s FrozenSlot, h uint64) uint64 {
	kend := uint64(s.KeyOff) + uint64(s.KeyLen)
	key := data[uint64(s.KeyOff):kend]
	mask := uint64(m.numGroups - 1)
	g := (h >> 7) & mask
	h2 := byte(h & 0x7f)
	firstEmpty := -1
	var probes uint64
	for probe := uint64(0); probe <= uint64(m.numGroups); probe++ {
		probes = probe + 1
		base := int(g) * groupSlots
		for j := range groupSlots {
			c := m.ctrl[base+j]
			if c == ctrlEmpty {
				if firstEmpty < 0 {
					firstEmpty = base + j
				}
				continue
			}
			if c == h2 {
				o := m.slots[base+j]
				okend := uint64(o.KeyOff) + uint64(o.KeyLen)
				if data[uint64(o.KeyOff):okend] == key {
					m.slots[base+j] = s
					return probes
				}
			}
		}
		if firstEmpty >= 0 {
			break
		}
		g = (g + probe + 1) & mask
	}
	m.ctrl[firstEmpty] = h2
	m.slots[firstEmpty] = s
	m.n++
	return probes
}

// NewFrozenMap builds a FrozenMap holding the entries of src.
func NewFrozenMap[K comparable, V any](src map[K]V, o *MapOptions[V]) (*FrozenMap[K, V], error) {
	b, err := NewFrozenBuilder[K, V](len(src), o)
	if err != nil {
		return nil, err
	}
	for k, v := range src {
		if err := b.Add(k, v); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// BuildFrozenMap builds a FrozenMap from a sequence of entries; later
// entries win over earlier entries with the same key.
func BuildFrozenMap[K comparable, V any](seq iter.Seq2[K, V], o *MapOptions[V]) (*FrozenMap[K, V], error) {
	b, err := NewFrozenBuilder[K, V](0, o)
	if err != nil {
		return nil, err
	}
	for k, v := range seq {
		if err := b.Add(k, v); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// EmptyFrozenMap returns an empty map.
func EmptyFrozenMap[K comparable, V any](o *MapOptions[V]) (*FrozenMap[K, V], error) {
	b, err := NewFrozenBuilder[K, V](0, o)
	if err != nil {
		return nil, err
	}
	return b.Finish()
}

// Len returns the number of entries.
func (m *FrozenMap[K, V]) Len() int {
	if m == nil {
		return 0
	}
	return m.n
}

// Seed returns the hash seed.
func (m *FrozenMap[K, V]) Seed() uint64 { return m.seed }

// matchByte returns a mask with the high bit set in every byte of g equal
// to b, possibly with false positives that callers re-check.
func matchByte(g uint64, b byte) uint64 {
	x := g ^ (lsbs * uint64(b))
	return (x - lsbs) &^ x & msbs
}

// lookup returns the slot index of the key with encoding key and hash h,
// or -1. Probing visits at most numGroups+1 groups, so even a corrupt table
// without empty slots cannot loop forever.
func (m *FrozenMap[K, V]) lookup(key string, h uint64) int {
	idx, _, _ := m.lookupWithWork(key, h)
	runtime.KeepAlive(m)
	return idx
}

// lookupWithWork returns the slot index of key (or -1), the number of
// groups probed and the number of key bytes compared.
func (m *FrozenMap[K, V]) lookupWithWork(key string, h uint64) (int, uint64, uint64) {
	ng := uint64(m.numGroups)
	if ng == 0 || uint64(len(m.ctrl)) < ng*groupSlots || uint64(len(m.slots)) < ng*groupSlots {
		return -1, 0, 0
	}
	var work uint64
	mask := ng - 1
	g := (h >> 7) & mask
	h2 := byte(h & 0x7f)
	dataLen := uint64(len(m.data))
	for probe := uint64(0); probe <= ng; probe++ {
		base := int(g) * groupSlots
		word := binary.LittleEndian.Uint64(m.ctrl[base : base+8])
		for match := matchByte(word, h2); match != 0; match &= match - 1 {
			shift := uint(bits.TrailingZeros64(match)) &^ 7
			if byte(word>>shift) != h2 {
				continue
			}
			i := base + int(shift>>3)
			s := m.slots[i]
			kend := uint64(s.KeyOff) + uint64(s.KeyLen)
			if kend <= dataLen && int(s.KeyLen) == len(key) {
				work += uint64(len(key))
				if m.data[uint64(s.KeyOff):kend] == key {
					return i, probe + 1, work
				}
			}
		}
		if word&msbs != 0 {
			return -1, probe + 1, work
		}
		g = (g + probe + 1) & mask
	}
	return -1, ng + 1, work
}

// lookupUint64 probes m for an 8-byte little-endian key without constructing
// a temporary key buffer.
func (m *FrozenMap[K, V]) lookupUint64(v, h uint64) int {
	ng := uint64(m.numGroups)
	if ng == 0 || uint64(len(m.ctrl)) < ng*groupSlots || uint64(len(m.slots)) < ng*groupSlots {
		runtime.KeepAlive(m)
		return -1
	}
	mask := ng - 1
	g := (h >> 7) & mask
	h2 := byte(h & 0x7f)
	dataLen := uint64(len(m.data))
	for probe := uint64(0); probe <= ng; probe++ {
		base := int(g) * groupSlots
		word := binary.LittleEndian.Uint64(m.ctrl[base : base+8])
		for match := matchByte(word, h2); match != 0; match &= match - 1 {
			shift := uint(bits.TrailingZeros64(match)) &^ 7
			if byte(word>>shift) != h2 {
				continue
			}
			i := base + int(shift>>3)
			s := m.slots[i]
			if s.KeyLen != 8 {
				continue
			}
			kend := uint64(s.KeyOff) + 8
			if kend <= dataLen && r8s(m.data[uint64(s.KeyOff):kend]) == v {
				runtime.KeepAlive(m)
				return i
			}
		}
		if word&msbs != 0 {
			runtime.KeepAlive(m)
			return -1
		}
		g = (g + probe + 1) & mask
	}
	runtime.KeepAlive(m)
	return -1
}

// lookupUint32 probes m for a 4-byte little-endian key without constructing
// a temporary key buffer.
func (m *FrozenMap[K, V]) lookupUint32(v uint32, h uint64) int {
	ng := uint64(m.numGroups)
	if ng == 0 || uint64(len(m.ctrl)) < ng*groupSlots || uint64(len(m.slots)) < ng*groupSlots {
		runtime.KeepAlive(m)
		return -1
	}
	mask := ng - 1
	g := (h >> 7) & mask
	h2 := byte(h & 0x7f)
	dataLen := uint64(len(m.data))
	for probe := uint64(0); probe <= ng; probe++ {
		base := int(g) * groupSlots
		word := binary.LittleEndian.Uint64(m.ctrl[base : base+8])
		for match := matchByte(word, h2); match != 0; match &= match - 1 {
			shift := uint(bits.TrailingZeros64(match)) &^ 7
			if byte(word>>shift) != h2 {
				continue
			}
			i := base + int(shift>>3)
			s := m.slots[i]
			if s.KeyLen != 4 {
				continue
			}
			kend := uint64(s.KeyOff) + 4
			if kend <= dataLen && uint32(r4s(m.data[uint64(s.KeyOff):kend])) == v {
				runtime.KeepAlive(m)
				return i
			}
		}
		if word&msbs != 0 {
			runtime.KeepAlive(m)
			return -1
		}
		g = (g + probe + 1) & mask
	}
	runtime.KeepAlive(m)
	return -1
}

func (m *FrozenMap[K, V]) find(k *K) int {
	if m == nil || m.n == 0 {
		return -1
	}
	switch m.kc.fast {
	case keyString:
		s := m.kc.str(k)
		return m.lookup(s, HashString(s, m.seed))
	case keyFixed8:
		v := m.kc.u64(k)
		return m.lookupUint64(v, HashUint64(v, m.seed))
	case keyFixed4:
		v := m.kc.u32(k)
		return m.lookupUint32(v, HashUint32(v, m.seed))
	}
	if m.kc.hasNaN(k) {
		return -1
	}
	var buf keyBuf
	key, err := m.kc.encode(&buf, k)
	if err != nil {
		return -1
	}
	return m.lookup(key, HashString(key, m.seed))
}

// Get returns the value stored for k. If V is a dictionary-backed Compact
// value whose Dict has been closed, decoding fails and Get returns (zero,
// false); do not close a Dict while a FrozenMap referencing it remains in use.
func (m *FrozenMap[K, V]) Get(k K) (V, bool) {
	i := m.find(&k)
	if i < 0 {
		runtime.KeepAlive(m)
		var zero V
		return zero, false
	}
	s := m.slots[i]
	vend := uint64(s.ValOff) + uint64(s.ValLen)
	if vend > uint64(len(m.data)) {
		runtime.KeepAlive(m)
		var zero V
		return zero, false
	}
	v, err := m.vc.Decode(m.data[uint64(s.ValOff):vend])
	runtime.KeepAlive(m)
	if err != nil {
		var zero V
		return zero, false
	}
	return v, true
}

// GetEncoded returns the encoded value stored for k. The result aliases the
// map's storage.
func (m *FrozenMap[K, V]) GetEncoded(k K) (string, bool) {
	i := m.find(&k)
	if i < 0 {
		runtime.KeepAlive(m)
		return "", false
	}
	s := m.slots[i]
	vend := uint64(s.ValOff) + uint64(s.ValLen)
	if vend > uint64(len(m.data)) {
		runtime.KeepAlive(m)
		return "", false
	}
	raw := m.data[uint64(s.ValOff):vend]
	runtime.KeepAlive(m)
	return raw, true
}

// Borrow looks up k and passes its raw encoded value directly to fn without
// copying, even when CloneStrings is enabled. The string passed to fn aliases
// the map's storage and must not be retained beyond the return of fn.
func (m *FrozenMap[K, V]) Borrow(k K, fn func(raw string)) bool {
	i := m.find(&k)
	if i < 0 {
		runtime.KeepAlive(m)
		return false
	}
	s := m.slots[i]
	vend := uint64(s.ValOff) + uint64(s.ValLen)
	if vend > uint64(len(m.data)) {
		runtime.KeepAlive(m)
		return false
	}
	fn(m.data[uint64(s.ValOff):vend])
	runtime.KeepAlive(m)
	return true
}

// Has reports whether k is present.
func (m *FrozenMap[K, V]) Has(k K) bool {
	ok := m.find(&k) >= 0
	runtime.KeepAlive(m)
	return ok
}

// All returns an iterator over all entries in table order.
func (m *FrozenMap[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if m == nil {
			return
		}
		defer runtime.KeepAlive(m)
		dataLen := uint64(len(m.data))
		limit := min(len(m.ctrl), len(m.slots))
		for i := range limit {
			c := m.ctrl[i]
			if c == ctrlEmpty || c > 0x7f {
				continue
			}
			s := m.slots[i]
			kend := uint64(s.KeyOff) + uint64(s.KeyLen)
			vend := uint64(s.ValOff) + uint64(s.ValLen)
			if kend > dataLen || vend > dataLen {
				continue
			}
			k, err := m.kc.decode(m.data[uint64(s.KeyOff):kend])
			if err != nil {
				continue
			}
			v, err := m.vc.Decode(m.data[uint64(s.ValOff):vend])
			if err != nil {
				continue
			}
			if !yield(k, v) {
				return
			}
		}
	}
}

// RawEntries returns an iterator over the canonical key encodings and value
// encodings of all entries. The strings alias the map's storage.
func (m *FrozenMap[K, V]) RawEntries() iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		if m == nil {
			return
		}
		defer runtime.KeepAlive(m)
		dataLen := uint64(len(m.data))
		limit := min(len(m.ctrl), len(m.slots))
		for i := range limit {
			c := m.ctrl[i]
			if c == ctrlEmpty || c > 0x7f {
				continue
			}
			s := m.slots[i]
			kend := uint64(s.KeyOff) + uint64(s.KeyLen)
			vend := uint64(s.ValOff) + uint64(s.ValLen)
			if kend > dataLen || vend > dataLen {
				continue
			}
			if !yield(m.data[uint64(s.KeyOff):kend], m.data[uint64(s.ValOff):vend]) {
				return
			}
		}
	}
}

// DecodeKey decodes a canonical key encoding returned by RawEntries.
func (m *FrozenMap[K, V]) DecodeKey(raw string) (K, error) { return m.kc.decode(raw) }

// ValueCodec returns the map's value codec.
func (m *FrozenMap[K, V]) ValueCodec() Codec[V] { return m.vc }

// FrozenSections is the persistent form of a FrozenMap.
type FrozenSections struct {
	Seed      uint64
	NumGroups uint32
	Len       uint32
	Ctrl      []byte
	Slots     []FrozenSlot
	Data      string
}

// Sections returns the pointer-free storage of m.
func (m *FrozenMap[K, V]) Sections() FrozenSections {
	return FrozenSections{Seed: m.seed, NumGroups: m.numGroups, Len: uint32(m.n), Ctrl: m.ctrl, Slots: m.slots, Data: m.data}
}

// FrozenMapFromSections reassembles a FrozenMap from its storage without
// copying. The geometry is always checked: the table must have a power of
// two number of groups and at least one empty slot. If validate is set,
// every slot is checked as well (see Validate), so that no lookup or
// iteration can panic or disagree with another; otherwise lookups on a
// corrupt table still terminate in bounded time.
func FrozenMapFromSections[K comparable, V any](sec FrozenSections, o *MapOptions[V], validate bool) (*FrozenMap[K, V], error) {
	if o == nil {
		o = &MapOptions[V]{}
	}
	oo := *o
	oo.Seed = sec.Seed
	if oo.Seed == 0 {
		return nil, fmt.Errorf("%w: zero map seed", ErrCorruptHeader)
	}
	_, kc, vc, err := resolveOptions[K, V](&oo)
	if err != nil {
		return nil, err
	}
	ng := uint64(sec.NumGroups)
	if ng == 0 || ng&(ng-1) != 0 || uint64(len(sec.Ctrl)) != ng*groupSlots || len(sec.Slots) != len(sec.Ctrl) || uint64(sec.Len) >= uint64(len(sec.Ctrl)) {
		return nil, fmt.Errorf("%w: frozen map geometry", ErrBoundsViolation)
	}
	m := &FrozenMap[K, V]{seed: sec.Seed, ctrl: sec.Ctrl, slots: sec.Slots, data: sec.Data, numGroups: sec.NumGroups, n: int(sec.Len), kc: kc, vc: vc}
	if validate {
		if err := m.Validate(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Validate checks every control byte and slot of m in O(N) time: spans must
// lie inside the data blob and their total byte length must not exceed the
// blob size, keys must be canonical encodings, values must decode (with
// ValidateEncoded, so that self-encoded values such as generated Compact
// values are verified completely), every control byte must match the hash
// of its key, the load factor must not exceed 7/8, the number of occupied
// slots must equal Len and leave at least one slot empty, and every key must
// be found by a lookup at its own slot within a linear probe budget. The
// last condition rules out duplicate keys and keys stored outside their
// probe sequence, so that Get and All always agree. The bytes of keys
// compared during these lookups are bounded by 32 times the data size.
func (m *FrozenMap[K, V]) Validate() error {
	defer runtime.KeepAlive(m)
	dataLen := uint64(len(m.data))
	full := 0
	var totalBytes uint64
	var totalProbes uint64
	probeBudget := uint64(8*m.n + 64)
	// Key comparisons are charged by length, so that many long keys sharing
	// a prefix and a control byte cannot make validation superlinear.
	workBudget := 32*dataLen + 65536
	for i, c := range m.ctrl {
		if c == ctrlEmpty {
			continue
		}
		if c > 0x7f {
			return fmt.Errorf("%w: control byte %#x", ErrBoundsViolation, c)
		}
		full++
		if full > m.n {
			return fmt.Errorf("%w: frozen map occupied slots exceed length %d", ErrBoundsViolation, m.n)
		}
		s := m.slots[i]
		kend := uint64(s.KeyOff) + uint64(s.KeyLen)
		vend := uint64(s.ValOff) + uint64(s.ValLen)
		if kend > dataLen || vend > dataLen {
			return fmt.Errorf("%w: frozen map slot %d", ErrBoundsViolation, i)
		}
		totalBytes += uint64(s.KeyLen) + uint64(s.ValLen)
		if totalBytes > dataLen {
			return fmt.Errorf("%w: frozen map overlapping spans exceed data size", ErrBoundsViolation)
		}
		key := m.data[uint64(s.KeyOff):kend]
		k, err := m.kc.decode(key)
		if err != nil {
			return fmt.Errorf("%w: frozen map key %d: %v", ErrBoundsViolation, i, err)
		}
		if !m.kc.isString {
			// Lookups encode keys canonically, so a stored key that is not
			// canonical (NaN, -0.0, a non-0/1 boolean) could be iterated
			// but never found.
			var buf keyBuf
			if enc, err := m.kc.encode(&buf, &k); err != nil || enc != key {
				return fmt.Errorf("%w: frozen map key %d is not canonical", ErrBoundsViolation, i)
			}
		}
		if err := ValidateEncoded(m.vc, m.data[uint64(s.ValOff):vend]); err != nil {
			return fmt.Errorf("%w: frozen map value %d: %v", ErrBoundsViolation, i, err)
		}
		h := HashString(key, m.seed)
		if byte(h&0x7f) != c {
			return fmt.Errorf("%w: frozen map control byte mismatch at %d", ErrBoundsViolation, i)
		}
		j, probes, work := m.lookupWithWork(key, h)
		totalProbes += probes
		if totalProbes > probeBudget {
			return fmt.Errorf("%w: frozen map probe budget exceeded", ErrBoundsViolation)
		}
		if work > workBudget {
			return fmt.Errorf("%w: frozen map key comparison budget exceeded", ErrBoundsViolation)
		}
		workBudget -= work
		if j != i {
			return fmt.Errorf("%w: frozen map key at slot %d is not reachable by lookup (found at %d)", ErrBoundsViolation, i, j)
		}
	}
	if full != m.n || full >= len(m.ctrl) {
		return fmt.Errorf("%w: frozen map length %d, %d occupied slots of %d", ErrBoundsViolation, m.n, full, len(m.ctrl))
	}
	if m.n > 0 && uint64(full)*8 > uint64(len(m.ctrl))*7 {
		return fmt.Errorf("%w: frozen map load factor %d/%d exceeds 7/8", ErrBoundsViolation, full, len(m.ctrl))
	}
	return nil
}

// WithFaultSafe calls fn with m and converts memory faults raised while it
// runs (for example from a memory-mapped file truncated by another process,
// see unsafemmap.OpenFrozenMap) into ErrMmapFault.
func (m *FrozenMap[K, V]) WithFaultSafe(fn func(*FrozenMap[K, V]) error) error {
	err := FaultSafe(func() error { return fn(m) })
	runtime.KeepAlive(m)
	return err
}

// FaultSafe calls fn with debug.SetPanicOnFault enabled and converts a
// memory fault raised by fn into an error wrapping ErrMmapFault. Other
// panics propagate unchanged.
func FaultSafe(fn func() error) (err error) {
	old := debug.SetPanicOnFault(true)
	defer func() {
		debug.SetPanicOnFault(old)
		if r := recover(); r != nil {
			if re, ok := r.(runtime.Error); ok {
				if _, fault := re.(interface{ Addr() uintptr }); fault {
					err = fmt.Errorf("%w: %v", ErrMmapFault, re)
					return
				}
			}
			panic(r)
		}
	}()
	return fn()
}

// CloneStrings returns a copy of s; it exists so that callers that decode
// from memory-mapped maps can detach results explicitly.
//
//go:noinline
func CloneStrings(s string) string { return strings.Clone(s) }
