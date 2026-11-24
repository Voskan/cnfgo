package cnfrt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDictNoPointers(t *testing.T) {
	if err := AssertNoPointers[dictSlot](); err != nil {
		t.Fatal(err)
	}
	if err := AssertNoPointers[atomic.Uint32](); err != nil {
		t.Fatal(err)
	}
}

func TestDictSeenTwiceAdmission(t *testing.T) {
	d := NewDict(1 << 20)
	defer d.Close()

	if tok := d.Token(); uint16(tok) == 0 || tok>>16 == 0 {
		t.Fatalf("Token() = %#x, want non-zero slot and nonce", tok)
	}
	if LookupDictToken(d.Token()) != d {
		t.Fatal("LookupDictToken did not return registered Dict")
	}

	// Unique strings must never be admitted on first sight.
	for i := range 2000 {
		s := fmt.Sprintf("unique-uid-%08d", i)
		if _, ok := d.Lookup(s); ok {
			t.Fatalf("unexpected hit for unseen string %q", s)
		}
		if _, ok := d.Intern(s); ok {
			// A hash collision in the 262k-entry filter is extremely rare over 2000 keys,
			// but check total admitted bytes remains negligible.
			continue
		}
	}
	st := d.Stats()
	if st.Entries > 20 {
		t.Fatalf("seen-twice filter admitted too many unique strings: %d", st.Entries)
	}

	// Repeated strings are admitted on second Intern and retrieved identically.
	repeated := []string{
		"prometheus.io/scrape",
		"registry.example.com/app/service:v1.2.3",
		"250m",
		"Ready",
		"TCP",
	}
	for _, s := range repeated {
		if _, ok := d.Intern(s); ok {
			t.Fatalf("expected first Intern(%q) to stay local", s)
		}
		sp, ok := d.Intern(s)
		if !ok {
			t.Fatalf("expected second Intern(%q) to admit into Dict", s)
		}
		if !sp.IsDict() {
			t.Fatalf("expected DictSpanBit set on %+v", sp)
		}
		if !d.ValidSpan(sp) {
			t.Fatalf("ValidSpan(%+v) = false", sp)
		}
		got, ok := d.Get(sp)
		if !ok || got != s {
			t.Fatalf("Get(%+v) = %q, %v; want %q, true", sp, got, ok, s)
		}
		if got := d.MustGet(sp); got != s {
			t.Fatalf("MustGet(%+v) = %q; want %q", sp, got, s)
		}
		sp2, ok := d.Lookup(s)
		if !ok || sp2 != sp {
			t.Fatalf("Lookup(%q) = %+v, %v; want %+v, true", s, sp2, ok, sp)
		}
	}

	// Empty string and invalid spans.
	if _, ok := d.Intern(""); ok {
		t.Fatal("empty string should not be interned")
	}
	if d.ValidSpan(Span32{}) || d.ValidSpan(NilSpan) || d.ValidSpan(Span32{Off: DictSpanBit | 999999, Len: 5}) {
		t.Fatal("expected invalid spans to be rejected")
	}

	tok := d.Token()
	sp, _ := d.Lookup(repeated[0])
	d.Close()
	if LookupDictToken(tok) != nil {
		t.Fatal("expected LookupDictToken to return nil after Close")
	}
	if !d.Closed() {
		t.Fatal("Closed() = false after Close")
	}
	if _, ok := d.Get(sp); ok {
		t.Fatal("Get succeeded on a closed Dict")
	}
	if _, ok := d.Intern(repeated[0]); ok {
		t.Fatal("Intern succeeded on a closed Dict")
	}
	func() {
		defer func() {
			if r := recover(); r != ErrUnknownDict {
				t.Fatalf("MustGet on a closed Dict panicked with %v, want ErrUnknownDict", r)
			}
		}()
		d.MustGet(sp)
	}()
	// Closing twice is safe.
	d.Close()
}

func TestDictBoundedCapacity(t *testing.T) {
	const limit = 2048
	d := NewDict(limit)
	defer d.Close()

	var admitted []struct {
		s  string
		sp Span32
	}
	for i := range 500 {
		s := fmt.Sprintf("repeated-payload-string-%04d-xxxxxxxxxxxxxxxxxxxx", i)
		d.Intern(s)
		if sp, ok := d.Intern(s); ok {
			admitted = append(admitted, struct {
				s  string
				sp Span32
			}{s, sp})
		}
	}
	st := d.Stats()
	if !st.Full {
		t.Fatalf("expected dict to become full; stats=%+v", st)
	}
	if st.MemBytes > MinDictMaxBytes {
		t.Fatalf("dict memory %d exceeded budget %d", st.MemBytes, MinDictMaxBytes)
	}
	if len(admitted) == 0 {
		t.Fatal("expected at least some strings admitted before full")
	}
	for _, item := range admitted {
		if got := d.MustGet(item.sp); got != item.s {
			t.Fatalf("MustGet(%+v) = %q, want %q", item.sp, got, item.s)
		}
	}
}

func TestDictConcurrent(t *testing.T) {
	d := NewDict(1 << 20)
	defer d.Close()

	const goroutines = 8
	const iters = 1000
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := range iters {
				common := fmt.Sprintf("common-label-%03d", i%128)
				if sp, ok := d.Intern(common); ok {
					if got, ok := d.Get(sp); !ok || got != common {
						t.Errorf("Get mismatch: got %q (%v), want %q", got, ok, common)
						return
					}
					if !d.ValidSpan(sp) {
						t.Errorf("ValidSpan false for %+v", sp)
						return
					}
				}
				unique := fmt.Sprintf("unique-%d-%d", gid, i)
				d.Intern(unique)
				if sp, ok := d.Lookup(common); ok {
					if d.MustGet(sp) != common {
						t.Errorf("MustGet mismatch")
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestBuilderWithDict(t *testing.T) {
	d := NewDict(1 << 20)
	defer d.Close()

	// Pre-warm "shared" twice so it's in Dict, leave "warm-on-second" once.
	d.Intern("shared")
	d.Intern("shared")
	d.Intern("warm-on-second")

	var b Builder
	b.Init(ModeDAG, 1, WithDict(d))
	b.MeasureString("shared")
	b.MeasureString("warm-on-second")
	b.MeasureString("unique-local")
	b.MeasureBytes([]byte("shared")) // bytes must stay local
	b.BeginEmit()

	spShared := b.String("shared")
	spWarm := b.String("warm-on-second")
	spLocal := b.String("unique-local")
	spBytes := b.Bytes([]byte("shared"))
	blob := b.StrBlob()

	if !b.UsedDict() {
		t.Fatal("expected UsedDict() == true")
	}
	if !spShared.IsDict() || d.MustGet(spShared) != "shared" {
		t.Fatalf("expected shared in dict, got %+v", spShared)
	}
	if !spWarm.IsDict() || d.MustGet(spWarm) != "warm-on-second" {
		t.Fatalf("expected warm-on-second admitted on pass 2, got %+v", spWarm)
	}
	if spLocal.IsDict() || blob[spLocal.Off:spLocal.End()] != "unique-local" {
		t.Fatalf("expected unique-local in local blob, got %+v", spLocal)
	}
	if spBytes.IsDict() || blob[spBytes.Off:spBytes.End()] != "shared" {
		t.Fatalf("expected Bytes to stay local, got %+v", spBytes)
	}
}

func TestSlimCompactRoundTripAndValidation(t *testing.T) {
	d := NewDict(1 << 20)
	defer d.Close()

	specs := []SlimSectionSpec{
		{Kind: SectionPool, ElemAlign: 8, Tag: 0, ElemSize: 8},
		{Kind: SectionTable, ElemAlign: 4, Tag: 1, ElemSize: 4},
		{Kind: SectionPool, ElemAlign: 2, Tag: 1, ElemSize: 2},
		{Kind: SectionStrBlob, ElemAlign: 1, Tag: 0, ElemSize: 1},
		{Kind: SectionExt, ElemAlign: 1, Tag: 0, ElemSize: 1},
	}

	root := make([]byte, 24)
	binary.LittleEndian.PutUint64(root[0:], 0x1122334455667788)
	binary.LittleEndian.PutUint64(root[8:], 0x99aabbccddeeff00)
	binary.LittleEndian.PutUint64(root[16:], 42)

	pool0 := make([]byte, 16)
	binary.LittleEndian.PutUint64(pool0[0:], 101)
	binary.LittleEndian.PutUint64(pool0[8:], 202)
	tbl1 := make([]byte, 8)
	binary.LittleEndian.PutUint32(tbl1[0:], 7)
	binary.LittleEndian.PutUint32(tbl1[4:], 9)
	strBlob := []byte("hello-slim-compact")
	ext := EncodeExt([]LocEntry{{Kind: LocFixed, Name: "EST", Offset: -18000}}, nil)

	secs := []Section{
		{Kind: SectionPool, ElemAlign: 8, Tag: 0, Count: 2, Data: pool0},
		{Kind: SectionTable, ElemAlign: 4, Tag: 1, Count: 2, Data: tbl1},
		{Kind: SectionStrBlob, ElemAlign: 1, Tag: 0, Count: uint32(len(strBlob)), Data: strBlob},
		{Kind: SectionExt, ElemAlign: 1, Tag: 0, Count: uint32(len(ext)), Data: ext},
	}

	const schema = uint64(0xdeadbeefcafebabe)
	const seed = uint64(0x123456789abcdef0)
	flags := uint8(SlimFlagDict | SlimFlagExt | SlimFlagCyclic | SlimFlagSeed)

	raw, err := AppendSlimCompact(nil, flags, root, d.Token(), seed, schema, specs, secs)
	if err != nil {
		t.Fatalf("AppendSlimCompact: %v", err)
	}
	if err := CheckSlimCompactPrefix(string(raw), 1+len(root), schema); err != nil {
		t.Fatalf("CheckSlimCompactPrefix: %v", err)
	}
	if err := CheckSlimCompactPrefix(string(raw), 1+len(root), schema^1); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("CheckSlimCompactPrefix with a foreign schema: %v, want ErrSchemaMismatch", err)
	}

	// Untrusted input may not reference a Dict unless it is allowed.
	if _, _, _, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyUntrusted, true, nil, nil); !errors.Is(err, ErrDictRequired) {
		t.Fatalf("untrusted parse without AllowDict: %v, want ErrDictRequired", err)
	}
	other := NewDict(1 << 20)
	if _, _, _, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyUntrusted, true, other, nil); !errors.Is(err, ErrDictRequired) {
		t.Fatalf("untrusted parse with another Dict: %v, want ErrDictRequired", err)
	}
	other.Close()
	if _, _, _, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyUntrusted, true, other, nil); !errors.Is(err, ErrUnknownDict) {
		t.Fatalf("untrusted parse with a closed Dict: %v, want ErrUnknownDict", err)
	}
	if _, gotDict, _, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyChecksum, true, nil, nil); err != nil || gotDict != d {
		t.Fatalf("trusted parse = %p, %v; want registered Dict", gotDict, err)
	}

	var infoBuf [8]SectionInfo
	h, gotDict, infos, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyUntrusted, true, d, infoBuf[:0])
	if err != nil {
		t.Fatalf("ParseSlimCompact: %v", err)
	}
	if gotDict != d {
		t.Fatalf("dict = %p, want %p", gotDict, d)
	}
	if h.SchemaHash != schema || h.HashSeed != seed || h.RootID != 1 {
		t.Fatalf("unexpected header: %+v", h)
	}
	if h.Flags != FlagCyclicAllowed|FlagHasDict {
		t.Fatalf("header flags = %#x, want %#x", h.Flags, FlagCyclicAllowed|FlagHasDict)
	}
	if len(infos) != 4 {
		t.Fatalf("len(infos) = %d, want 4", len(infos))
	}
	for _, in := range infos {
		if in.Off%uint64(in.ElemAlign) != 0 {
			t.Fatalf("section kind %d tag %d offset %d not aligned to %d", in.Kind, in.Tag, in.Off, in.ElemAlign)
		}
	}
	if in, ok := FindSection(infos, SectionStrBlob, 0); !ok || !bytes.Equal(SectionData(raw, in), strBlob) {
		t.Fatalf("strBlob mismatch")
	}

	// CRC corruption must be rejected under VerifyChecksum.
	badCRC := append([]byte(nil), raw...)
	badCRC[len(badCRC)-1] ^= 0xff
	if _, _, _, err := ParseSlimCompact(badCRC, len(root), schema, 0, specs, VerifyChecksum, true, d, nil); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}

	// Schema mismatch must be rejected.
	if _, _, _, err := ParseSlimCompact(raw, len(root), schema^1, 0, specs, VerifyUntrusted, true, d, nil); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("expected ErrSchemaMismatch, got %v", err)
	}

	// Trailing garbage must be rejected.
	withTrail := append(append([]byte(nil), raw[:len(raw)-4]...), 0x00)
	withTrail = binary.LittleEndian.AppendUint32(withTrail, CRC32C(withTrail))
	if _, _, _, err := ParseSlimCompact(withTrail, len(root), schema, 0, specs, VerifyUntrusted, true, d, nil); !errors.Is(err, ErrBoundsViolation) {
		t.Fatalf("expected ErrBoundsViolation on trailing byte, got %v", err)
	}

	// Closing dict causes ErrUnknownDict.
	d.Close()
	if err := CheckSlimCompactPrefix(string(raw), 1+len(root), schema); !errors.Is(err, ErrUnknownDict) {
		t.Fatalf("expected ErrUnknownDict from CheckSlimCompactPrefix, got %v", err)
	}
	if _, _, _, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyUntrusted, true, d, nil); !errors.Is(err, ErrUnknownDict) {
		t.Fatalf("expected ErrUnknownDict from ParseSlimCompact, got %v", err)
	}
	if _, _, _, err := ParseSlimCompact(raw, len(root), schema, 0, specs, VerifyChecksum, true, nil, nil); !errors.Is(err, ErrUnknownDict) {
		t.Fatalf("expected ErrUnknownDict from a trusted ParseSlimCompact, got %v", err)
	}

	// A new Dict reusing the slot must not resolve the old token.
	reuse := NewDict(1 << 20)
	defer reuse.Close()
	if uint16(reuse.Token()) == uint16(d.Token()) {
		if err := CheckSlimCompactPrefix(string(raw), 1+len(root), schema); !errors.Is(err, ErrUnknownDict) {
			t.Fatalf("stale token resolved through a reused slot: %v", err)
		}
	}
}

func FuzzParseSlimCompact(f *testing.F) {
	specs := []SlimSectionSpec{
		{Kind: SectionPool, ElemAlign: 8, Tag: 0, ElemSize: 8},
		{Kind: SectionTable, ElemAlign: 4, Tag: 1, ElemSize: 4},
		{Kind: SectionStrBlob, ElemAlign: 1, Tag: 0, ElemSize: 1},
		{Kind: SectionExt, ElemAlign: 1, Tag: 0, ElemSize: 1},
	}
	root := make([]byte, 16)
	const schema = uint64(0x1122334455667788)
	seed1, _ := AppendSlimCompact(nil, 0, root, 0, DefaultSeed, schema, specs, []Section{
		{Kind: SectionStrBlob, ElemAlign: 1, Tag: 0, Count: 5, Data: []byte("hello")},
	})
	seed2, _ := AppendSlimCompact(nil, 0, root, 0, DefaultSeed, schema, specs, nil)
	f.Add(seed1)
	f.Add(seed2)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = CheckSlimCompactPrefix(string(data), 1+len(root), schema)
		_, _, _, _ = ParseSlimCompact(data, len(root), schema, 0, specs, VerifyUntrusted, true, nil, nil)
	})
}

func TestDictSlotReuse(t *testing.T) {
	seen := map[uint64]bool{}
	for i := range 100_000 {
		d := NewDict(MinDictMaxBytes)
		tok := d.Token()
		if tok == 0 {
			t.Fatalf("iteration %d: registry exhausted", i)
		}
		if seen[tok] {
			t.Fatalf("iteration %d: token %#x reused", i, tok)
		}
		seen[tok] = true
		d.Close()
		if LookupDictToken(tok) != nil {
			t.Fatalf("iteration %d: closed token still resolves", i)
		}
	}
}

func TestDictRegistryFullFallsBack(t *testing.T) {
	var open []*Dict
	defer func() {
		for _, d := range open {
			d.Close()
		}
	}()
	for {
		d := NewDict(MinDictMaxBytes)
		open = append(open, d)
		if d.Token() == 0 {
			break
		}
		if len(open) > 1<<16 {
			t.Fatal("registry admitted more than 65535 open dictionaries")
		}
	}
	d := open[len(open)-1]
	for range 2 {
		if _, ok := d.Intern("fallback"); ok {
			t.Fatal("unregistered Dict admitted a string")
		}
	}
	if _, ok := d.Lookup("fallback"); ok {
		t.Fatal("unregistered Dict found a string")
	}
	open = open[:len(open)-1]
	open[0].Close()
	again := NewDict(MinDictMaxBytes)
	open = append(open, again)
	if again.Token() == 0 {
		t.Fatal("released slot was not reused")
	}
}

func TestDictMemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a full 32 MiB dictionary")
	}
	for _, size := range []int{4, 8, 32, 120} {
		d := NewDict(32 << 20)
		var buf [256]byte
		for i := 0; !d.Stats().Full; i++ {
			s := buf[:size]
			binary.LittleEndian.PutUint32(s, uint32(i))
			for j := 4; j < size; j++ {
				s[j] = 'x'
			}
			d.Intern(string(s))
			d.Intern(string(s))
			if i > 40_000_000 {
				t.Fatalf("size %d: Dict never became full: %+v", size, d.Stats())
			}
		}
		st := d.Stats()
		var real uint64
		for _, ch := range *d.chunks.Load() {
			real += uint64(len(ch.str))
		}
		real += uint64(len(d.slots))*dictSlotBytes + uint64(len(d.seen))*dictSeenBytes
		if st.MemBytes != real || real > 32<<20 {
			t.Fatalf("size %d: accounted %d, real %d, budget %d", size, st.MemBytes, real, 32<<20)
		}
		if 2*real < 32<<20 {
			t.Fatalf("size %d: Dict sealed at %d bytes, under half of its budget", size, real)
		}
		t.Logf("size %3d: %d entries, %d string bytes, %d bytes of memory", size, st.Entries, st.Bytes, real)
		d.Close()
	}
}

func TestDictSeedPerInstance(t *testing.T) {
	a, b := NewDict(0), NewDict(0)
	defer a.Close()
	defer b.Close()
	if a.seed == b.seed || a.seed == DefaultSeed {
		t.Fatalf("dictionaries share a hash seed %#x", a.seed)
	}
}

func TestBuilderClosedDict(t *testing.T) {
	d := NewDict(0)
	d.Close()
	var err error
	func() {
		defer func() { Recover(recover(), &err) }()
		var b Builder
		b.Init(ModeDAG, 1, WithDict(d))
	}()
	if !errors.Is(err, ErrUnknownDict) {
		t.Fatalf("Init with a closed Dict: %v, want ErrUnknownDict", err)
	}
}

func TestDictValidCapturedSpan(t *testing.T) {
	d := NewDict(2 << 20)
	defer d.Close()

	// Populate chunk 0.
	first := "chunk-zero-entry"
	d.Intern(first)
	sp0, ok := d.Intern(first)
	if !ok {
		t.Fatal("failed to intern chunk 0 string")
	}
	captured := d.Chunks()
	if len(captured) != 1 {
		t.Fatalf("captured chunks = %d, want 1", len(captured))
	}
	if !d.ValidCapturedSpan(captured, sp0) {
		t.Fatal("ValidCapturedSpan rejected a valid chunk-0 span")
	}
	// Uncommitted bytes inside chunk 0 must be rejected even though len(captured[0]) covers them.
	uncommitted := Span32{Off: DictSpanBit | uint32(len(first)+10), Len: 4}
	if d.ValidCapturedSpan(captured, uncommitted) {
		t.Fatal("ValidCapturedSpan accepted uncommitted offset")
	}

	// Grow the dictionary into chunk 1 after captured was taken.
	var sp1 Span32
	for i := range 2000 {
		s := fmt.Sprintf("fill-%05d-%s", i, bytes.Repeat([]byte{'a'}, 60))
		d.Intern(s)
		if sp, ok := d.Intern(s); ok && (sp.Off&^DictSpanBit)>>dictChunkShift >= 1 {
			sp1 = sp
			break
		}
	}
	if sp1.Len == 0 {
		t.Fatal("dictionary did not allocate a second chunk")
	}
	if !d.ValidSpan(sp1) {
		t.Fatal("ValidSpan rejected live chunk-1 span")
	}
	if d.ValidCapturedSpan(captured, sp1) {
		t.Fatal("ValidCapturedSpan accepted chunk-1 span absent from captured chunks")
	}
	if !d.ValidCapturedSpan(d.Chunks(), sp1) {
		t.Fatal("ValidCapturedSpan rejected chunk-1 span present in refreshed chunks")
	}
	d.Close()
	if d.ValidCapturedSpan(captured, sp0) {
		t.Fatal("ValidCapturedSpan accepted span on a closed Dict")
	}
}

func TestDictProgressiveChunkGrowth(t *testing.T) {
	cases := []struct {
		budget     int
		minEntries int
	}{
		{64 << 10, 1000},
		{512 << 10, 4000},
		{2 << 20, 4000},
	}
	for _, tc := range cases {
		d := NewDict(tc.budget)
		var admitted []struct {
			s  string
			sp Span32
		}
		for i := range 5000 {
			s := fmt.Sprintf("shared-key-%08d", i)
			d.Intern(s)
			if sp, ok := d.Intern(s); ok {
				admitted = append(admitted, struct {
					s  string
					sp Span32
				}{s, sp})
			}
		}
		st := d.Stats()
		if st.MemBytes > uint64(tc.budget) {
			d.Close()
			t.Fatalf("budget %d: MemBytes %d exceeded limit", tc.budget, st.MemBytes)
		}
		if st.Entries < tc.minEntries {
			d.Close()
			t.Fatalf("budget %d: admitted only %d entries (want >= %d): %+v", tc.budget, st.Entries, tc.minEntries, st)
		}
		for _, item := range admitted {
			if got, ok := d.Get(item.sp); !ok || got != item.s {
				d.Close()
				t.Fatalf("budget %d: Get(%+v) = %q, %v; want %q", tc.budget, item.sp, got, ok, item.s)
			}
		}
		d.Close()
	}
}

func TestBuilderDictClosedBetweenPasses(t *testing.T) {
	modes := []struct {
		name string
		opt  FreezeOption
	}{
		{"DefaultDedup", nil},
		{"WithoutStringDedup", WithoutStringDedup()},
		{"WithExactStringDedup", WithExactStringDedup()},
	}
	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDict(64 << 10)
			d.Intern("shared-alpha")
			d.Intern("shared-alpha")
			d.Intern("shared-beta")
			d.Intern("shared-beta")

			opts := []FreezeOption{WithDict(d)}
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			var err error
			func() {
				defer func() { Recover(recover(), &err) }()
				var b Builder
				b.Init(ModeDAG, 1, opts...)
				b.MeasureString("shared-alpha")
				b.MeasureString("shared-beta")
				b.BeginEmit()
				d.Close()
				b.String("shared-alpha")
				b.String("shared-beta")
				_ = b.StrBlob()
			}()
			if !errors.Is(err, ErrUnknownDict) {
				t.Fatalf("Dict closed between passes: got %v, want ErrUnknownDict", err)
			}
		})
	}
}

func TestDictSealTrimsUnwrittenTail(t *testing.T) {
	d := NewDict(MinDictMaxBytes)
	defer d.Close()

	var hdr [9]byte
	hdr[0] = SlimCompactVersion | SlimFlagDict
	binary.LittleEndian.PutUint64(hdr[1:], d.Token())
	raw := string(hdr[:])

	mustPanic := func(label string, off, n uint32) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: ResolveDictSpan(%#x, %d) did not panic", label, off, n)
			}
		}()
		_ = ResolveDictSpan(raw, 1, off, n)
	}

	// Admit one string and verify unwritten tail bytes panic before Full.
	const first = "alpha-entry"
	d.Intern(first)
	firstSpan, ok := d.Intern(first)
	if !ok {
		t.Fatal("failed to admit first string")
	}
	if d.Stats().Full {
		t.Fatal("dict unexpectedly full after one entry")
	}
	if got := ResolveDictSpan(raw, 1, firstSpan.Off, firstSpan.Len); got != first {
		t.Fatalf("before Full: ResolveDictSpan = %q, want %q", got, first)
	}
	chunksBefore := *d.chunks.Load()
	usedBefore := chunksBefore[0].used.Load()
	if usedBefore >= uint32(len(chunksBefore[0].str)) {
		t.Fatalf("chunk 0 has no unwritten tail before Full: used=%d cap=%d", usedBefore, len(chunksBefore[0].str))
	}
	mustPanic("before Full", DictSpanBit|usedBefore, 1)

	// Fill until Full while leaving an unwritten tail in at least one chunk.
	for i := range 200 {
		s := fmt.Sprintf("seal-tail-entry-%03d-%s", i, bytes.Repeat([]byte{'x'}, 180))
		d.Intern(s)
		d.Intern(s)
		if d.Stats().Full {
			break
		}
	}
	if !d.Stats().Full {
		t.Fatalf("dict did not become Full: %+v", d.Stats())
	}
	if got := ResolveDictSpan(raw, 1, firstSpan.Off, firstSpan.Len); got != first {
		t.Fatalf("after Full: ResolveDictSpan = %q, want %q", got, first)
	}

	chunks := *d.chunks.Load()
	strs := *d.strs.Load()
	checkedTail := false
	for ci, ch := range chunks {
		used := ch.used.Load()
		if ch.buf != nil {
			t.Fatalf("chunk %d buf not cleared after seal", ci)
		}
		if len(strs[ci]) != int(used) {
			t.Fatalf("chunk %d sealed str len = %d, want used = %d", ci, len(strs[ci]), used)
		}
		if int(used) < len(ch.str) {
			checkedTail = true
			mustPanic(fmt.Sprintf("after Full chunk %d tail", ci), DictSpanBit|(uint32(ci)<<dictChunkShift)|used, 1)
		}
	}
	if !checkedTail {
		t.Fatal("no chunk had an unwritten tail to verify after Full")
	}
}
