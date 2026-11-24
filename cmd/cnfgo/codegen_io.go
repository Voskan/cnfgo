package main

import (
	"fmt"
	"sort"
	"strings"
)

// --- .cnf persistence ----------------------------------------------------------

func (g *gen) genIO() {
	sc := g.sc
	first := sc.prefix + sc.roots[0].name
	s := "s *" + g.snap
	hash := "SchemaHash" + first

	g.p("func (%s) appendSections(secs []cnfrt.Section) []cnfrt.Section {", s)
	for _, n := range sc.tableNodes() {
		g.p("\tif len(s.nodes%s) > 0 {", n.name)
		g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionTable, ElemAlign: %d, Tag: %d, Count: uint32(len(s.nodes%s)), Data: cnfrt.ViewSliceLE(s.nodes%s)})", n.flat.align, n.tbl, n.name, n.name)
		g.p("\t}")
	}
	for _, p := range sc.pools {
		g.p("\tif len(s.%s) > 0 {", p.field)
		g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionPool, ElemAlign: %d, Tag: %d, Count: uint32(len(s.%s)), Data: cnfrt.ViewSliceLE(s.%s)})", p.align, p.idx, p.field, p.field)
		g.p("\t}")
	}
	g.p("\tif len(s.strBlob) > 0 {")
	g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionStrBlob, ElemAlign: 1, Count: uint32(len(s.strBlob)), Data: cnfrt.UnsafeStringBytes(s.strBlob)})")
	g.p("\t}")
	g.p("\tif ext := cnfrt.EncodeExt(s.locEntries, s.zoned); ext != nil {")
	g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionExt, ElemAlign: 4, Count: uint32(len(ext)), Data: ext})")
	g.p("\t}")
	g.p("\treturn secs")
	g.p("}")
	g.p("")
	g.p("func (%s) sections() []cnfrt.Section {", s)
	g.p("\treturn s.appendSections(make([]cnfrt.Section, 0, %d))", g.maxSecs)
	g.p("}")
	g.p("")
	g.p("func (%s) header(extra uint32) cnfrt.Header {", s)
	g.p("\treturn cnfrt.Header{Flags: s.flags | extra, SchemaHash: %s, HashSeed: s.seed, RootTable: s.rootTable, RootID: s.rootID}", hash)
	g.p("}")
	g.p("")
	g.p("// WriteTo writes the snapshot as a .cnf image to w. It returns")
	g.p("// cnfrt.ErrClosed if s is a closed memory-mapped snapshot, or")
	g.p("// cnfrt.ErrDictRequired if s references a process-local cnfrt.Dict.")
	g.p("func (%s) WriteTo(w io.Writer) (int64, error) {", s)
	g.p("\tif s.mapping != nil && s.mapping.Closed() {")
	g.p("\t\treturn 0, cnfrt.ErrClosed")
	g.p("\t}")
	g.p("\tif s.flags&cnfrt.FlagHasDict != 0 {")
	g.p("\t\treturn 0, cnfrt.ErrDictRequired")
	g.p("\t}")
	g.p("\tvar secBuf [%d]cnfrt.Section", g.maxSecs)
	g.p("\tn, err := cnfrt.WriteImage(w, s.header(0), s.appendSections(secBuf[:0]))")
	g.p("\truntime.KeepAlive(s)")
	g.p("\treturn n, err")
	g.p("}")
	g.p("")
	g.p("// MarshalCNF returns the snapshot encoded as a .cnf image. It returns")
	g.p("// cnfrt.ErrClosed if s is a closed memory-mapped snapshot, or")
	g.p("// cnfrt.ErrDictRequired if s references a process-local cnfrt.Dict.")
	g.p("func (%s) MarshalCNF() ([]byte, error) {", s)
	g.p("\tif s.mapping != nil && s.mapping.Closed() {")
	g.p("\t\treturn nil, cnfrt.ErrClosed")
	g.p("\t}")
	g.p("\tif s.flags&cnfrt.FlagHasDict != 0 {")
	g.p("\t\treturn nil, cnfrt.ErrDictRequired")
	g.p("\t}")
	g.p("\tvar secBuf [%d]cnfrt.Section", g.maxSecs)
	g.p("\th, secs := s.header(0), s.appendSections(secBuf[:0])")
	g.p("\tout, err := cnfrt.AppendImage(make([]byte, 0, cnfrt.ImageSize(h, secs)), h, secs)")
	g.p("\truntime.KeepAlive(s)")
	g.p("\treturn out, err")
	g.p("}")
	g.p("")
	g.p("// Write%sAtomic writes s to path atomically (temporary file, fsync, rename,", g.snap)
	g.p("// directory fsync), so concurrent Map%sFile calls never observe a partial", first)
	g.p("// file.")
	g.p("func Write%sAtomic(path string, s *%s) error {", g.snap, g.snap)
	g.p("\treturn cnfrt.WriteSnapshotAtomic(path, func(w io.Writer) error {")
	g.p("\t\t_, err := s.WriteTo(w)")
	g.p("\t\treturn err")
	g.p("\t})")
	g.p("}")
	g.p("")
	g.p("// Map%sFile memory-maps the .cnf file at path read-only and returns a", first)
	g.p("// snapshot over it. With cnfrt.VerifyTrustedHeader only the header and")
	g.p("// section geometry are checked; cnfrt.VerifyChecksum also verifies all")
	g.p("// section checksums. cnfrt.VerifyUntrusted copies the file to the heap,")
	g.p("// unmaps it and validates every reference, so arbitrary input is safe to")
	g.p("// read; the returned snapshot is then not memory-mapped.")
	g.p("//")
	g.p("// String getters of memory-mapped snapshots return copies. The mapping is")
	g.p("// released by Close or, failing that, when the snapshot becomes unreachable.")
	g.p("func Map%sFile(path string, mode cnfrt.VerifyMode) (*%s, error) {", first, g.snap)
	g.p("\tm, err := unsafemmap.Open(path)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tif mode >= cnfrt.VerifyUntrusted {")
	g.p("\t\tvar data []byte")
	g.p("\t\terr := unsafemmap.FaultSafe(func() error {")
	g.p("\t\t\tdata = cnfrt.AlignedCopy(m.Bytes())")
	g.p("\t\t\treturn nil")
	g.p("\t\t})")
	g.p("\t\tif cerr := m.Close(); err == nil {")
	g.p("\t\t\terr = cerr")
	g.p("\t\t}")
	g.p("\t\tif err != nil {")
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t\treturn track%s(%s(data, mode))", g.snap, g.load)
	g.p("\t}")
	g.p("\tvar s *%s", g.snap)
	g.p("\terr = unsafemmap.FaultSafe(func() (err error) {")
	g.p("\t\ts, err = %s(m.Bytes(), mode)", g.load)
	g.p("\t\treturn err")
	g.p("\t})")
	g.p("\tif err != nil {")
	g.p("\t\tm.Close()")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\tmapped := m.IsMapped()")
	g.p("\ts.mapping, s.isMmap, s.cloneStrs = m, mapped, mapped")
	g.p("\tunsafemmap.AttachCleanup(s, m)")
	g.p("\treturn trackShared%s(s, nil)", g.snap)
	g.p("}")
	g.p("")
	for _, r := range sc.roots[1:] {
		rn := sc.prefix + r.name
		g.p("// Map%sFile memory-maps the .cnf file at path read-only and returns a", rn)
		g.p("// snapshot over it; see Map%sFile.", first)
		g.p("func Map%sFile(path string, mode cnfrt.VerifyMode) (*%s, error) {", rn, g.snap)
		g.p("\treturn Map%sFile(path, mode)", first)
		g.p("}")
		g.p("")
		g.p("// Write%sSnapshotAtomic writes s to path atomically; see Write%sAtomic.", rn, g.snap)
		g.p("func Write%sSnapshotAtomic(path string, s *%s) error {", rn, g.snap)
		g.p("\treturn Write%sAtomic(path, s)", g.snap)
		g.p("}")
		g.p("")
	}
	g.p("// Unmarshal%s decodes a .cnf image. Tables and strings of the result alias", g.snap)
	g.p("// data, which must not be modified while the snapshot is in use, except with")
	g.p("// cnfrt.VerifyUntrusted, which validates an aligned private copy.")
	g.p("func Unmarshal%s(data []byte, mode cnfrt.VerifyMode) (*%s, error) {", g.snap, g.snap)
	g.p("\tif mode >= cnfrt.VerifyUntrusted {")
	g.p("\t\treturn track%s(%s(cnfrt.AlignedCopy(data), mode))", g.snap, g.load)
	g.p("\t}")
	g.p("\treturn trackShared%s(%s(data, mode))", g.snap, g.load)
	g.p("}")
	g.p("")
	g.p("func %s(data []byte, mode cnfrt.VerifyMode) (*%s, error) {", g.load, g.snap)
	g.p("\th, infos, err := cnfrt.ParseHeader(data, mode, %s)", hash)
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\treturn %s(data, h, infos, mode)", g.build)
	g.p("}")
	g.p("")
	g.p("// %s assembles a snapshot from the parsed sections of an image.", g.build)
	g.p("func %s(data []byte, h cnfrt.Header, infos []cnfrt.SectionInfo, mode cnfrt.VerifyMode) (*%s, error) {", g.build, g.snap)
	g.p("\tvar err error")
	g.p("\ts := &%s{flags: h.Flags, seed: h.HashSeed, rootTable: h.RootTable, rootID: h.RootID}", g.snap)
	g.p("\tfor _, in := range infos {")
	g.p("\t\tswitch {")
	for _, n := range sc.tableNodes() {
		g.p("\t\tcase in.Kind == cnfrt.SectionTable && in.Tag == %d:", n.tbl)
		g.p("\t\t\ts.nodes%s, err = unsafemmap.Cast[%s](data, in)", n.name, n.flatName)
	}
	for _, p := range sc.pools {
		g.p("\t\tcase in.Kind == cnfrt.SectionPool && in.Tag == %d:", p.idx)
		g.p("\t\t\ts.%s, err = unsafemmap.Cast[%s](data, in)", p.field, p.flatExpr)
	}
	g.p("\t\tcase in.Kind == cnfrt.SectionStrBlob && in.Tag == 0:")
	g.p("\t\t\ts.strBlob = unsafemmap.String(data, in)")
	g.p("\t\tcase in.Kind == cnfrt.SectionExt && in.Tag == 0:")
	g.p("\t\t\ts.locEntries, s.zoned, err = cnfrt.DecodeExt(cnfrt.SectionData(data, in))")
	g.p("\t\t\ts.locs = (&cnfrt.LocTable{Entries: s.locEntries}).Locations()")
	g.p("\t\tdefault:")
	g.p("\t\t\terr = fmt.Errorf(\"%%w: unexpected section kind %%d tag %%d\", cnfrt.ErrCorruptHeader, in.Kind, in.Tag)")
	g.p("\t\t}")
	g.p("\t\tif err != nil {")
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tswitch s.rootTable {")
	for _, r := range sc.roots {
		g.p("\tcase %d:", r.tbl)
		g.p("\t\tif s.rootID == 0 || uint64(s.rootID) > uint64(len(s.nodes%s)) {", r.name)
		g.p("\t\t\treturn nil, fmt.Errorf(\"%%w: root ID %%d\", cnfrt.ErrBoundsViolation, s.rootID)")
		g.p("\t\t}")
	}
	g.p("\tdefault:")
	g.p("\t\treturn nil, fmt.Errorf(\"%%w: root table %%d\", cnfrt.ErrSchemaMismatch, s.rootTable)")
	g.p("\t}")
	g.p("\tif mode >= cnfrt.VerifyUntrusted {")
	g.p("\t\tif err := s.validate(); err != nil {")
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn s, nil")
	g.p("}")
	g.p("")
}

// --- Compact ---------------------------------------------------------------------

// flatOffset returns the offset of the named field in fs.
func flatOffset(fs *flatStruct, name string) int {
	for _, f := range fs.fields {
		if f.name == name {
			return f.off
		}
	}
	panic("flat field not found: " + name)
}

// compactReserved lists the methods of generated Compact types. Root fields
// whose getters would collide with them are rejected by the analyzer.
var compactReserved = map[string]bool{
	"IsZero": true, "UsesDict": true, "Detach": true, "Raw": true, "Equal": true,
	"View": true, "TryView": true, "Snapshot": true, "Unpack": true,
	"SizeCNF": true, "AppendCNF": true, "DecodeCNF": true, "CodecCNF": true,
	"SchemaHashCNF": true,
}

// compactRead returns an expression decoding a flat value of shape sh
// stored at constant offset off of c.cnfRaw, or "" if sh has no direct
// reader.
func (g *gen) compactRead(sh *shape, off int) string {
	u := func(bits, o int) string {
		return fmt.Sprintf("binary.LittleEndian.Uint%d([]byte(c.cnfRaw[%d:%d]))", bits, o, o+bits/8)
	}
	scalar := func(info *scalarInfo, o int) string {
		switch info.flat {
		case "uint8":
			return fmt.Sprintf("c.cnfRaw[%d]", o)
		case "int8":
			return fmt.Sprintf("int8(c.cnfRaw[%d])", o)
		case "uint16", "uint32", "uint64":
			return u(info.size*8, o)
		case "int16", "int32", "int64":
			return fmt.Sprintf("int%d(%s)", info.size*8, u(info.size*8, o))
		case "float32":
			return "math.Float32frombits(" + u(32, o) + ")"
		case "float64":
			return "math.Float64frombits(" + u(64, o) + ")"
		case "[2]float32":
			return fmt.Sprintf("[2]float32{math.Float32frombits(%s), math.Float32frombits(%s)}", u(32, o), u(32, o+4))
		case "[2]float64":
			return fmt.Sprintf("[2]float64{math.Float64frombits(%s), math.Float64frombits(%s)}", u(64, o), u(64, o+8))
		}
		panic("compact scalar " + info.flat)
	}
	switch sh.kind {
	case kScalar:
		return g.scalarRead(sh, scalar(sh.sc, off))
	case kTime:
		return fmt.Sprintf("c.cnfTime(%d)", off)
	case kAddr:
		return fmt.Sprintf("c.cnfAddr(%d)", off)
	case kPrefix:
		return fmt.Sprintf("cnfrt.DecodePrefix(cnfrt.CompactFlatPrefix(c.cnfRaw, %d))", off)
	case kAddrPort:
		return fmt.Sprintf("netip.AddrPortFrom(c.cnfAddr(%d), %s)", off, u(16, off+20))
	case kArrayInline:
		if sh.n > 64 {
			return ""
		}
		var parts []string
		for i := int64(0); i < sh.n; i++ {
			parts = append(parts, g.scalarRead(sh.elem, scalar(sh.elem.sc, off+int(i)*sh.elem.size)))
		}
		return sh.goExpr + "{" + strings.Join(parts, ", ") + "}"
	}
	return ""
}

func (g *gen) genCompactArrayLoop(sh *shape, off int) {
	g.p("\traw := c.cnfRaw[%d:%d]", off, off+sh.size)
	if sh.elem.goExpr == "uint8" || sh.elem.goExpr == "byte" {
		g.p("\tcopy(out[:], raw)")
		return
	}
	u := func(bits int, oExpr string) string {
		return fmt.Sprintf("binary.LittleEndian.Uint%d([]byte(raw[%s:%s+%d]))", bits, oExpr, oExpr, bits/8)
	}
	oExpr := "i"
	if sh.elem.size > 1 {
		oExpr = fmt.Sprintf("i*%d", sh.elem.size)
	}
	var elemRaw string
	switch sh.elem.sc.flat {
	case "uint8":
		elemRaw = "raw[i]"
	case "int8":
		elemRaw = "int8(raw[i])"
	case "uint16", "uint32", "uint64":
		elemRaw = u(sh.elem.sc.size*8, oExpr)
	case "int16", "int32", "int64":
		elemRaw = fmt.Sprintf("int%d(%s)", sh.elem.sc.size*8, u(sh.elem.sc.size*8, oExpr))
	case "float32":
		elemRaw = "math.Float32frombits(" + u(32, oExpr) + ")"
	case "float64":
		elemRaw = "math.Float64frombits(" + u(64, oExpr) + ")"
	case "[2]float32":
		elemRaw = fmt.Sprintf("[2]float32{math.Float32frombits(%s), math.Float32frombits(%s)}", u(32, oExpr), u(32, oExpr+"+4"))
	case "[2]float64":
		elemRaw = fmt.Sprintf("[2]float64{math.Float64frombits(%s), math.Float64frombits(%s)}", u(64, oExpr), u(64, oExpr+"+8"))
	default:
		panic("compact scalar " + sh.elem.sc.flat)
	}
	g.p("\tfor i := range out {")
	g.p("\t\tout[i] = %s", g.scalarRead(sh.elem, elemRaw))
	g.p("\t}")
}

type slimSpecEntry struct {
	kind      string
	kindNum   int
	elemAlign int
	tag       int
	elemSize  int
}

func (g *gen) slimSpecsFor(root *node) ([]slimSpecEntry, bool) {
	sc := g.sc
	includeExtraRoot := root.selfRef || sc.crossSCC
	var specs []slimSpecEntry
	for _, n := range sc.tableNodes() {
		if n == root && !includeExtraRoot {
			continue
		}
		specs = append(specs, slimSpecEntry{
			kind:      "cnfrt.SectionTable",
			kindNum:   1,
			elemAlign: n.flat.align,
			tag:       n.tbl,
			elemSize:  n.flat.size,
		})
	}
	for _, p := range sc.pools {
		specs = append(specs, slimSpecEntry{
			kind:      "cnfrt.SectionPool",
			kindNum:   2,
			elemAlign: p.align,
			tag:       p.idx,
			elemSize:  p.size,
		})
	}
	specs = append(specs,
		slimSpecEntry{kind: "cnfrt.SectionStrBlob", kindNum: 3, elemAlign: 1, tag: 0, elemSize: 1},
		slimSpecEntry{kind: "cnfrt.SectionExt", kindNum: 4, elemAlign: 1, tag: 0, elemSize: 1},
	)
	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].elemAlign != specs[j].elemAlign {
			return specs[i].elemAlign > specs[j].elemAlign
		}
		if specs[i].kindNum != specs[j].kindNum {
			return specs[i].kindNum < specs[j].kindNum
		}
		return specs[i].tag < specs[j].tag
	})
	return specs, includeExtraRoot
}

func (g *gen) genCompact(n *node) {
	sc := g.sc
	ct := "Compact" + sc.prefix + n.name
	fn := sc.prefix + n.name
	lcCt := lowerFirst(ct)
	hash := "SchemaHash" + sc.prefix + sc.roots[0].name
	rootSize := n.flat.size
	hdr := 1 + rootSize
	specs, includeExtraRoot := g.slimSpecsFor(n)

	hasTime, hasAddr := false, false
	for _, f := range n.fields {
		t := f.sh
		if t.kind == kOptional {
			t = t.elem
		}
		switch t.kind {
		case kTime:
			hasTime = true
		case kAddr, kAddrPort:
			hasAddr = true
		}
	}

	g.p("// %s is a 16-byte immutable value holding a complete frozen %s in a", ct, n.name)
	g.p("// single string, so a map or slice of %s values contains one pointer per", ct)
	g.p("// value. Scalar, string, time and address fields of the root are read")
	g.p("// directly at constant offsets without allocating; everything else is")
	g.p("// reachable through View.")
	g.p("//")
	g.p("// Layout (slim format 0x03): a flags byte, the root's single %d-byte flat", rootSize)
	g.p("// record, the string blob offset, optional dictToken / ext / seed headers,")
	g.p("// the 64-bit schema hash, a section presence bitmask, unsigned varint")
	g.p("// counts for non-empty sections, payloads ordered by descending alignment")
	g.p("// (8, 4, 2, 1), and a trailing 4-byte CRC32C.")
	g.p("type %s struct {", ct)
	g.p("\tcnfRaw string")
	g.p("}")
	g.p("")
	g.p("var %sSlimSpecs = [%d]cnfrt.SlimSectionSpec{", lcCt, len(specs))
	for _, sp := range specs {
		g.p("\t{Kind: %s, ElemAlign: %d, Tag: %d, ElemSize: %d},", sp.kind, sp.elemAlign, sp.tag, sp.elemSize)
	}
	g.p("}")
	g.p("")
	g.p("// SchemaHashCNF implements cnfrt.SchemaHasher.")
	g.p("func (%s) SchemaHashCNF() uint64 { return %s }", ct, hash)
	g.p("")
	defMode := "cnfrt.ModeDAG"
	if sc.treeDefault {
		defMode = "cnfrt.ModeTree"
	}
	g.p("// Pack%s freezes src (as Freeze%s does) into a %s.", fn, fn, ct)
	g.p("func Pack%s(src *%s, opts ...cnfrt.FreezeOption) (%s, error) {", fn, n.goExpr, ct)
	g.p("\treturn pack%s(%s, src, opts)", fn, defMode)
	g.p("}")
	g.p("")
	g.p("// PackTree%s freezes src as a tree (as FreezeTree%s does) into a %s.", fn, fn, ct)
	g.p("func PackTree%s(src *%s, opts ...cnfrt.FreezeOption) (%s, error) {", fn, n.goExpr, ct)
	g.p("\treturn pack%s(cnfrt.ModeTree, src, opts)", fn)
	g.p("}")
	g.p("")
	g.p("// PackCyclic%s freezes src permitting directed cycles (as FreezeCyclic%s", fn, fn)
	g.p("// does) into a %s.", ct)
	g.p("func PackCyclic%s(src *%s, opts ...cnfrt.FreezeOption) (%s, error) {", fn, n.goExpr, ct)
	g.p("\treturn pack%s(cnfrt.ModeCyclic, src, opts)", fn)
	g.p("}")
	g.p("")
	g.p("// pack%s freezes src into the pooled scratch snapshot of a freezer and", fn)
	g.p("// encodes it, so that the only allocation is the encoded value itself.")
	g.p("func pack%s(mode cnfrt.FreezeMode, src *%s, opts []cnfrt.FreezeOption) (c %s, err error) {", fn, n.goExpr, ct)
	g.p("\tif src == nil {")
	g.p("\t\treturn %s{}, cnfrt.ErrNilRoot", ct)
	g.p("\t}")
	g.p("\tif mode != cnfrt.ModeTree {")
	g.p("\t\tcnfrt.Escape(src)")
	g.p("\t}")
	g.p("\tf := %sPool.Get().(*%s)", g.fz, g.fz)
	g.p("\tdefer func() {")
	g.p("\t\tcnfrt.Recover(recover(), &err)")
	g.p("\t\tif err != nil {")
	g.p("\t\t\tc = %s{}", ct)
	g.p("\t\t}")
	g.p("\t\tf.reset()")
	g.p("\t\t%sPool.Put(f)", g.fz)
	g.p("\t}()")
	g.p("\tf.b.Init(mode, %d, opts...)", len(sc.tableNodes()))
	g.p("\tf.scratch = true")
	g.p("\tf.b.ReuseStrBuf(f.strBuf)")
	g.p("\tf.mn%d(src)", n.tbl)
	g.p("\tf.allocate()")
	g.p("\tid := f.en%d(src)", n.tbl)
	g.p("\tf.finish()")
	g.p("\truntime.KeepAlive(src)")
	g.p("\tif id != 1 {")
	g.p("\t\treturn %s{}, fmt.Errorf(\"%%w: unexpected compact root ID %%d\", cnfrt.ErrBoundsViolation, id)", ct)
	g.p("\t}")
	g.p("\ts := f.s")
	g.p("\tvar secBuf [%d]cnfrt.Section", len(specs))
	g.p("\tsecs := secBuf[:0]")
	for _, tn := range sc.tableNodes() {
		if tn == n {
			if includeExtraRoot {
				g.p("\tif len(s.nodes%s) > 1 {", tn.name)
				g.p("\t\textra := s.nodes%s[1:]", tn.name)
				g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionTable, ElemAlign: %d, Tag: %d, Count: uint32(len(extra)), Data: cnfrt.ViewSliceLE(extra)})", tn.flat.align, tn.tbl)
				g.p("\t}")
			}
			continue
		}
		g.p("\tif len(s.nodes%s) > 0 {", tn.name)
		g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionTable, ElemAlign: %d, Tag: %d, Count: uint32(len(s.nodes%s)), Data: cnfrt.ViewSliceLE(s.nodes%s)})", tn.flat.align, tn.tbl, tn.name, tn.name)
		g.p("\t}")
	}
	var hashIdxChecks []string
	for _, p := range sc.pools {
		g.p("\tif len(s.%s) > 0 {", p.field)
		g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionPool, ElemAlign: %d, Tag: %d, Count: uint32(len(s.%s)), Data: cnfrt.ViewSliceLE(s.%s)})", p.align, p.idx, p.field, p.field)
		g.p("\t}")
		if p.hashIdx {
			hashIdxChecks = append(hashIdxChecks, "len(s."+p.field+") > 0")
		}
	}
	g.p("\tif len(s.strBlob) > 0 {")
	g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionStrBlob, ElemAlign: 1, Count: uint32(len(s.strBlob)), Data: cnfrt.UnsafeStringBytes(s.strBlob)})")
	g.p("\t}")
	g.p("\tif ext := cnfrt.EncodeExt(s.locEntries, s.zoned); ext != nil {")
	g.p("\t\tsecs = append(secs, cnfrt.Section{Kind: cnfrt.SectionExt, ElemAlign: 1, Count: uint32(len(ext)), Data: ext})")
	g.p("\t}")
	g.p("\tvar flags uint8")
	g.p("\tvar dictToken uint64")
	g.p("\tif s.flags&cnfrt.FlagHasDict != 0 {")
	g.p("\t\tflags |= cnfrt.SlimFlagDict")
	g.p("\t\tdictToken = s.dict.Token()")
	g.p("\t}")
	g.p("\tif s.flags&cnfrt.FlagCyclicAllowed != 0 {")
	g.p("\t\tflags |= cnfrt.SlimFlagCyclic")
	g.p("\t}")
	if len(hashIdxChecks) > 0 {
		g.p("\tif s.seed != cnfrt.DefaultSeed && (%s) {", strings.Join(hashIdxChecks, " || "))
		g.p("\t\tflags |= cnfrt.SlimFlagSeed")
		g.p("\t}")
	}
	g.p("\tbuf, err := cnfrt.AppendSlimCompact(nil, flags, cnfrt.ViewSliceLE(s.nodes%s[:1]), dictToken, s.seed, %s, %sSlimSpecs[:], secs)", n.name, hash, lcCt)
	g.p("\tf.strBuf = f.b.StrBuf()")
	g.p("\tif err != nil {")
	g.p("\t\treturn %s{}, err", ct)
	g.p("\t}")
	g.p("\treturn %s{cnfRaw: cnfrt.UnsafeSealBytes(buf)}, nil", ct)
	g.p("}")
	g.p("")
	g.p("// Borrow%s validates the compact prefix of raw and returns a %s that", ct, ct)
	g.p("// aliases raw without copying. It is intended for zero-copy reads inside")
	g.p("// callbacks such as SlabMap.Borrow and FrozenMap.Borrow; the returned value")
	g.p("// must not be retained beyond the lifetime of raw.")
	g.p("func Borrow%s(raw string) (%s, error) {", ct, ct)
	g.p("\ts, err := cnfrt.CheckSlimCompactString(raw, %d, %s)", hdr, hash)
	g.p("\treturn %s{cnfRaw: s}, err", ct)
	g.p("}")
	g.p("")
	g.p("// Borrow%sBytes is Borrow%s for a byte slice, without copying. The", ct, ct)
	g.p("// returned %s aliases raw: it is valid only while raw is neither", ct)
	g.p("// modified nor reused, typically for the duration of a SlabMap.Borrow")
	g.p("// callback, and must not be retained beyond it. Use Parse%s on a copy", ct)
	g.p("// to keep a value.")
	g.p("func Borrow%sBytes(raw []byte) (%s, error) {", ct, ct)
	g.p("\ts, err := cnfrt.CheckSlimCompactBytes(raw, %d, %s)", hdr, hash)
	g.p("\treturn %s{cnfRaw: s}, err", ct)
	g.p("}")
	g.p("")
	g.p("// Parse%s checks raw (as produced by Raw) and returns it as a %s. The", ct, ct)
	g.p("// embedded image is verified according to mode. With cnfrt.VerifyUntrusted")
	g.p("// a value that references a cnfrt.Dict is rejected with")
	g.p("// cnfrt.ErrDictRequired unless that Dict is passed with cnfrt.AllowDict.")
	g.p("func Parse%s(raw string, mode cnfrt.VerifyMode, opts ...cnfrt.ParseOption) (%s, error) {", ct, ct)
	g.p("\tc := %s{cnfRaw: raw}", ct)
	g.p("\tif _, err := c.cnfLoad(mode, true, cnfrt.AllowedDict(opts)); err != nil {")
	g.p("\t\treturn %s{}, err", ct)
	g.p("\t}")
	g.p("\treturn c, nil")
	g.p("}")
	g.p("")
	g.p("// cnfCheck verifies the structure of the compact prefix (version, size,")
	g.p("// schema hash, dictionary registration and the offsets used by direct")
	g.p("// getters).")
	g.p("func (c %s) cnfCheck() error {", ct)
	g.p("\treturn cnfrt.CheckSlimCompactPrefix(c.cnfRaw, %d, %s)", hdr, hash)
	g.p("}")
	g.p("")
	g.p("func (c %s) cnfU32(off int) uint32 {", ct)
	g.p("\treturn binary.LittleEndian.Uint32([]byte(c.cnfRaw[off : off+4]))")
	g.p("}")
	g.p("")
	if hasTime || hasAddr {
		g.p("func (c %s) cnfExt() string {", ct)
		g.p("\ts := c.cnfRaw")
		g.p("\tif len(s) < %d || s[0]&cnfrt.SlimFlagExt == 0 {", hdr+12)
		g.p("\t\treturn \"\"")
		g.p("\t}")
		g.p("\tp := %d", hdr+4)
		g.p("\tif s[0]&cnfrt.SlimFlagDict != 0 {")
		g.p("\t\tp += cnfrt.SlimDictTokenSize")
		g.p("\t}")
		g.p("\th := s[p : p+8]")
		g.p("\toff := int(binary.LittleEndian.Uint32([]byte(h[0:4])))")
		g.p("\tl := int(binary.LittleEndian.Uint32([]byte(h[4:8])))")
		g.p("\treturn s[off : off+l]")
		g.p("}")
		g.p("")
	}
	if hasTime {
		g.p("func (c %s) cnfTime(off int) time.Time {", ct)
		g.p("\tb := c.cnfRaw[off : off+16]")
		g.p("\tsec := int64(binary.LittleEndian.Uint64([]byte(b[0:8])))")
		g.p("\tmeta := binary.LittleEndian.Uint64([]byte(b[8:16]))")
		g.p("\tif (meta>>30)&3 == cnfrt.LocUTC {")
		g.p("\t\treturn time.Unix(sec, int64(meta&0x3fffffff)).UTC()")
		g.p("\t}")
		g.p("\treturn cnfrt.DecodeCompactTime(sec, meta, c.cnfExt())")
		g.p("}")
		g.p("")
	}
	if hasAddr {
		g.p("func (c %s) cnfAddr(off int) netip.Addr {", ct)
		g.p("\tf := cnfrt.CompactFlatAddr(c.cnfRaw, off)")
		g.p("\tif f.Kind != cnfrt.AddrV6Zoned {")
		g.p("\t\treturn cnfrt.DecodeAddr(f, nil)")
		g.p("\t}")
		g.p("\treturn cnfrt.DecodeCompactAddr(f, c.cnfExt())")
		g.p("}")
		g.p("")
	}
	g.p("func (c %s) cnfLoad(mode cnfrt.VerifyMode, checkCRC bool, allow *cnfrt.Dict) (*%s, error) {", ct, g.snap)
	g.p("\tdata := cnfrt.UnsafeStringBytes(c.cnfRaw)")
	g.p("\tif mode >= cnfrt.VerifyUntrusted {")
	g.p("\t\tdata = cnfrt.AlignedCopy(data)")
	g.p("\t}")
	g.p("\tvar infoBuf [%d]cnfrt.SectionInfo", len(specs))
	g.p("\th, dict, infos, err := cnfrt.ParseSlimCompact(data, %d, %s, %d, %sSlimSpecs[:], mode, checkCRC, allow, infoBuf[:0])", rootSize, hash, n.tbl, lcCt)
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\ts := &%s{flags: h.Flags, seed: h.HashSeed, rootTable: h.RootTable, rootID: h.RootID, dict: dict, dictStrs: dict.Chunks()}", g.snap)
	g.p("\tif err := cnfrt.DecodeSliceLE(s.inlineRoot%s[:], data[1:%d]); err != nil {", n.name, hdr)
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\ts.nodes%s = s.inlineRoot%s[:]", n.name, n.name)
	g.p("\tfor _, in := range infos {")
	g.p("\t\tswitch {")
	for _, tn := range sc.tableNodes() {
		if tn == n {
			if includeExtraRoot {
				g.p("\t\tcase in.Kind == cnfrt.SectionTable && in.Tag == %d:", tn.tbl)
				g.p("\t\t\textra, castErr := unsafemmap.Cast[%s](data, in)", tn.flatName)
				g.p("\t\t\tif castErr != nil {")
				g.p("\t\t\t\treturn nil, castErr")
				g.p("\t\t\t}")
				g.p("\t\t\tnodes := make([]%s, 1+len(extra))", tn.flatName)
				g.p("\t\t\tnodes[0] = s.inlineRoot%s[0]", tn.name)
				g.p("\t\t\tcopy(nodes[1:], extra)")
				g.p("\t\t\ts.nodes%s = nodes", tn.name)
			}
			continue
		}
		g.p("\t\tcase in.Kind == cnfrt.SectionTable && in.Tag == %d:", tn.tbl)
		g.p("\t\t\ts.nodes%s, err = unsafemmap.Cast[%s](data, in)", tn.name, tn.flatName)
	}
	for _, p := range sc.pools {
		g.p("\t\tcase in.Kind == cnfrt.SectionPool && in.Tag == %d:", p.idx)
		g.p("\t\t\ts.%s, err = unsafemmap.Cast[%s](data, in)", p.field, p.flatExpr)
	}
	g.p("\t\tcase in.Kind == cnfrt.SectionStrBlob && in.Tag == 0:")
	g.p("\t\t\ts.strBlob = unsafemmap.String(data, in)")
	g.p("\t\tcase in.Kind == cnfrt.SectionExt && in.Tag == 0:")
	g.p("\t\t\ts.locEntries, s.zoned, err = cnfrt.DecodeExt(cnfrt.SectionData(data, in))")
	g.p("\t\t\tif err == nil {")
	g.p("\t\t\t\ts.locs = (&cnfrt.LocTable{Entries: s.locEntries}).Locations()")
	g.p("\t\t\t}")
	g.p("\t\tdefault:")
	g.p("\t\t\terr = fmt.Errorf(\"%%w: unexpected section kind %%d tag %%d\", cnfrt.ErrCorruptHeader, in.Kind, in.Tag)")
	g.p("\t\t}")
	g.p("\t\tif err != nil {")
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\tif mode >= cnfrt.VerifyUntrusted {")
	g.p("\t\tif err := s.validate(); err != nil {")
	g.p("\t\t\treturn nil, err")
	g.p("\t\t}")
	g.p("\t}")
	g.p("\treturn s, nil")
	g.p("}")
	g.p("")
	g.p("// IsZero reports whether c is the zero value.")
	g.p("func (c %s) IsZero() bool { return c.cnfRaw == \"\" }", ct)
	g.p("")
	g.p("// UsesDict reports whether c stores any strings in a process-local cnfrt.Dict.")
	g.p("func (c %s) UsesDict() bool { return len(c.cnfRaw) > 0 && c.cnfRaw[0]&cnfrt.SlimFlagDict != 0 }", ct)
	g.p("")
	g.p("// Detach returns a self-contained %s with all dictionary strings", ct)
	g.p("// materialized into its local string blob. If c does not use a dictionary,")
	g.p("// Detach returns c. By default the result keeps the hash seed of c")
	g.p("// (DefaultSeed unless c carries an explicit seed) and time.Local, so")
	g.p("// detaching the same value twice yields identical bytes; caller opts")
	g.p("// override those defaults except that WithDict is always cleared so the")
	g.p("// result never references a cnfrt.Dict. It returns cnfrt.ErrUnknownDict")
	g.p("// if the dictionary of c has been closed.")
	g.p("func (c %s) Detach(opts ...cnfrt.FreezeOption) (%s, error) {", ct, ct)
	g.p("\tif !c.UsesDict() {")
	g.p("\t\treturn c, nil")
	g.p("\t}")
	g.p("\tv, err := c.Unpack()")
	g.p("\tif err != nil || v == nil {")
	g.p("\t\treturn %s{}, err", ct)
	g.p("\t}")
	g.p("\tmerged := make([]cnfrt.FreezeOption, 0, len(opts)+3)")
	g.p("\tmerged = append(merged, cnfrt.WithHashSeed(cnfrt.SlimCompactSeed(c.cnfRaw, %d)), cnfrt.WithPreserveLocal())", hdr)
	g.p("\tmerged = append(merged, opts...)")
	g.p("\tmerged = append(merged, cnfrt.WithDict(nil))")
	g.p("\tmode := cnfrt.ModeDAG")
	g.p("\tif c.cnfRaw[0]&cnfrt.SlimFlagCyclic != 0 {")
	g.p("\t\tmode = cnfrt.ModeCyclic")
	g.p("\t}")
	g.p("\treturn pack%s(mode, v, merged)", fn)
	g.p("}")
	g.p("")
	g.p("// Raw returns the encoded value. It can be stored and later passed to")
	g.p("// Parse%s.", ct)
	g.p("func (c %s) Raw() string { return c.cnfRaw }", ct)
	g.p("")
	g.p("// Equal reports whether c and o hold identical encodings.")
	g.p("func (c %s) Equal(o %s) bool { return c.cnfRaw == o.cnfRaw }", ct, ct)
	g.p("")
	g.p("// View returns a view of the root over the embedded image. The image is")
	g.p("// not copied, but a snapshot header is allocated; the direct getters of")
	g.p("// %s do not allocate. View panics if c is corrupt or references a closed", ct)
	g.p("// cnfrt.Dict (use TryView to get an error instead), and returns the zero")
	g.p("// view for the zero value.")
	g.p("func (c %s) View() %s {", ct, n.viewName)
	g.p("\tif c.cnfRaw == \"\" {")
	g.p("\t\treturn %s{}", n.viewName)
	g.p("\t}")
	g.p("\ts, err := c.cnfLoad(cnfrt.VerifyTrustedHeader, false, nil)")
	g.p("\tif err != nil {")
	g.p("\t\tpanic(err)")
	g.p("\t}")
	g.p("\treturn s.view%d(s.rootID)", n.tbl)
	g.p("}")
	g.p("")
	g.p("// TryView is like View but returns an error instead of panicking:")
	g.p("// cnfrt.ErrUnknownDict if c references a closed cnfrt.Dict, or a header")
	g.p("// error if c is corrupt.")
	g.p("func (c %s) TryView() (%s, error) {", ct, n.viewName)
	g.p("\tif c.cnfRaw == \"\" {")
	g.p("\t\treturn %s{}, nil", n.viewName)
	g.p("\t}")
	g.p("\tif err := c.cnfCheck(); err != nil {")
	g.p("\t\treturn %s{}, err", n.viewName)
	g.p("\t}")
	g.p("\ts, err := c.cnfLoad(cnfrt.VerifyTrustedHeader, false, nil)")
	g.p("\tif err != nil {")
	g.p("\t\treturn %s{}, err", n.viewName)
	g.p("\t}")
	g.p("\treturn s.view%d(s.rootID), nil", n.tbl)
	g.p("}")
	g.p("")
	g.p("// Snapshot loads the embedded image as a snapshot, verified according to")
	g.p("// mode; see Parse%s for the options.", ct)
	g.p("func (c %s) Snapshot(mode cnfrt.VerifyMode, opts ...cnfrt.ParseOption) (*%s, error) {", ct, g.snap)
	g.p("\treturn c.cnfLoad(mode, true, cnfrt.AllowedDict(opts))")
	g.p("}")
	g.p("")
	g.p("// Unpack rebuilds the original %s. It returns cnfrt.ErrUnknownDict if c", n.name)
	g.p("// references a closed cnfrt.Dict.")
	g.p("func (c %s) Unpack() (*%s, error) {", ct, n.goExpr)
	g.p("\tif c.cnfRaw == \"\" {")
	g.p("\t\treturn nil, nil")
	g.p("\t}")
	g.p("\ts, err := c.Snapshot(cnfrt.VerifyTrustedHeader)")
	g.p("\tif err != nil {")
	g.p("\t\treturn nil, err")
	g.p("\t}")
	g.p("\treturn s.view%d(s.rootID).Unfreeze()", n.tbl)
	g.p("}")
	g.p("")
	g.p("// SizeCNF implements cnfrt.SelfCodec.")
	g.p("func (c %s) SizeCNF() int { return len(c.cnfRaw) }", ct)
	g.p("")
	g.p("// AppendCNF implements cnfrt.SelfCodec.")
	g.p("func (c %s) AppendCNF(dst []byte) []byte { return append(dst, c.cnfRaw...) }", ct)
	g.p("")
	g.p("// DecodeCNF implements cnfrt.SelfDecoder. It checks the compact prefix;")
	g.p("// use Parse%s to verify the embedded image as well. src is retained.", ct)
	g.p("func (c *%s) DecodeCNF(src string) error {", ct)
	g.p("\ts, err := cnfrt.CheckSlimCompactString(src, %d, %s)", hdr, hash)
	g.p("\tif err == nil {")
	g.p("\t\tc.cnfRaw = s")
	g.p("\t}")
	g.p("\treturn err")
	g.p("}")
	g.p("")
	codec := lcCt + "Codec"
	g.p("// CodecCNF implements cnfrt.CodecProvider. Maps such as cnfrt.FrozenMap and")
	g.p("// cnfrt.SlabMap use the returned codec for %s values by default.", ct)
	g.p("func (%s) CodecCNF() cnfrt.Codec[%s] { return %s{} }", ct, ct, codec)
	g.p("")
	g.p("// %s is the cnfrt.Codec of %s. It also implements", codec, ct)
	g.p("// cnfrt.UntrustedValidator, verifying the embedded image completely and")
	g.p("// rejecting values that reference a cnfrt.Dict, and cnfrt.AlignedCodec,")
	g.p("// so that stored values keep their image 8-byte aligned.")
	g.p("type %s struct{}", codec)
	g.p("")
	g.p("func (%s) Size(c %s) int { return len(c.cnfRaw) }", codec, ct)
	g.p("")
	g.p("func (%s) Append(dst []byte, c %s) []byte { return append(dst, c.cnfRaw...) }", codec, ct)
	g.p("")
	g.p("func (%s) Decode(src string) (%s, error) {", codec, ct)
	g.p("\ts, err := cnfrt.CheckSlimCompactString(src, %d, %s)", hdr, hash)
	g.p("\treturn %s{cnfRaw: s}, err", ct)
	g.p("}")
	g.p("")
	g.p("func (%s) ValidateUntrusted(src string) error {", codec)
	g.p("\tif src == \"\" {")
	g.p("\t\treturn nil")
	g.p("\t}")
	g.p("\t_, err := Parse%s(src, cnfrt.VerifyUntrusted)", ct)
	g.p("\treturn err")
	g.p("}")
	g.p("")
	g.p("func (%s) AlignCNF() int { return 8 }", codec)
	g.p("")
	for _, f := range n.fields {
		sh := f.sh
		target := sh
		if sh.kind == kOptional {
			target = sh.elem
		}
		if target.isZeroArray() {
			if target.kind != kArrayInline {
				continue
			}
			if sh.kind == kOptional {
				pOff := 1 + flatOffset(n.flat, "PresentBits")
				test := fmt.Sprintf("c.cnfRaw[%d]&(1<<%d) != 0", pOff+f.optBit/8, f.optBit%8)
				g.p("// %s returns the %s field of the root and whether it is set.", f.getter, f.name)
				g.p("func (c %s) %s() (%s, bool) {", ct, f.getter, target.viewType)
				g.p("\tif c.cnfRaw == \"\" || !(%s) {", test)
				g.p("\t\treturn %s{}, false", target.viewType)
				g.p("\t}")
				g.p("\treturn %s{}, true", target.viewType)
				g.p("}")
			} else {
				g.p("// %s returns the %s field of the root without allocating.", f.getter, f.name)
				g.p("func (c %s) %s() %s { return %s{} }", ct, f.getter, sh.viewType, sh.viewType)
			}
			g.p("")
			continue
		}
		off := 1 + flatOffset(n.flat, f.flatName)
		if target.kind == kString || target.kind == kBytes || target.kind == kCodec {
			wrapStr := func(expr string) string {
				if target.kind == kString && target.goExpr != "string" {
					return target.goExpr + "(" + expr + ")"
				}
				return expr
			}
			switch {
			case sh.kind == kOptional && target.kind == kCodec:
				pOff := 1 + flatOffset(n.flat, "PresentBits")
				g.p("// %s returns the %s field of the root and whether it is set.", f.getter, f.name)
				g.p("func (c %s) %s() (%s, bool) {", ct, f.getter, target.viewType)
				g.p("\ts := c.cnfRaw")
				g.p("\tif len(s) < %d || s[%d]&(1<<%d) == 0 {", hdr+4, pOff+f.optBit/8, f.optBit%8)
				g.p("\t\treturn %s{}, false", target.viewType)
				g.p("\t}")
				g.p("\to, l := cnfrt.U32S(s[%d:]), cnfrt.U32S(s[%d:])", off, off+4)
				g.p("\tvar raw string")
				g.p("\tif o >= cnfrt.DictSpanBit {")
				g.p("\t\tif l != 0 {")
				g.p("\t\t\traw = cnfrt.ResolveDictSpan(s, %d, o, l)", hdr+4)
				g.p("\t\t}")
				g.p("\t} else {")
				g.p("\t\ti := int(cnfrt.U32S(s[%d:])) + int(o)", hdr)
				g.p("\t\traw = s[i : i+int(l)]")
				g.p("\t}")
				g.p("\treturn %s{Raw: raw}, true", target.viewType)
				g.p("}")
			case sh.kind == kOptional:
				pOff := 1 + flatOffset(n.flat, "PresentBits")
				g.p("// %s returns the %s field of the root and whether it is set.", f.getter, f.name)
				g.p("func (c %s) %s() (%s, bool) {", ct, f.getter, target.viewType)
				g.p("\ts := c.cnfRaw")
				g.p("\tif len(s) < %d || s[%d]&(1<<%d) == 0 {", hdr+4, pOff+f.optBit/8, f.optBit%8)
				g.p("\t\treturn \"\", false")
				g.p("\t}")
				g.p("\to, l := cnfrt.U32S(s[%d:]), cnfrt.U32S(s[%d:])", off, off+4)
				g.p("\tif o >= cnfrt.DictSpanBit {")
				g.p("\t\tif l == 0 {")
				g.p("\t\t\treturn \"\", true")
				g.p("\t\t}")
				g.p("\t\treturn %s, true", wrapStr(fmt.Sprintf("cnfrt.ResolveDictSpan(s, %d, o, l)", hdr+4)))
				g.p("\t}")
				g.p("\ti := int(cnfrt.U32S(s[%d:])) + int(o)", hdr)
				g.p("\treturn %s, true", wrapStr("s[i : i+int(l)]"))
				g.p("}")
			case sh.kind == kCodec:
				g.p("// %s decodes the %s field of the root.", f.getter, f.name)
				g.p("func (c %s) %s() (%s, error) {", ct, f.getter, sh.goExpr)
				g.p("\ts := c.cnfRaw")
				g.p("\tif len(s) < %d {", hdr+4)
				g.p("\t\tvar zero %s", sh.goExpr)
				g.p("\t\treturn zero, nil")
				g.p("\t}")
				g.p("\to, l := cnfrt.U32S(s[%d:]), cnfrt.U32S(s[%d:])", off, off+4)
				g.p("\tvar raw string")
				g.p("\tif o >= cnfrt.DictSpanBit {")
				g.p("\t\tif l != 0 {")
				g.p("\t\t\traw = cnfrt.ResolveDictSpan(s, %d, o, l)", hdr+4)
				g.p("\t\t}")
				g.p("\t} else {")
				g.p("\t\ti := int(cnfrt.U32S(s[%d:])) + int(o)", hdr)
				g.p("\t\traw = s[i : i+int(l)]")
				g.p("\t}")
				g.p("\treturn %s{Raw: raw}.Decode()", sh.viewType)
				g.p("}")
			default:
				g.p("// %s returns the %s field of the root without allocating.", f.getter, f.name)
				g.p("func (c %s) %s() %s {", ct, f.getter, sh.viewType)
				g.p("\ts := c.cnfRaw")
				g.p("\tif len(s) < %d {", hdr+4)
				g.p("\t\treturn \"\"")
				g.p("\t}")
				g.p("\to, l := cnfrt.U32S(s[%d:]), cnfrt.U32S(s[%d:])", off, off+4)
				g.p("\tif o >= cnfrt.DictSpanBit {")
				g.p("\t\tif l == 0 {")
				g.p("\t\t\treturn \"\"")
				g.p("\t\t}")
				g.p("\t\treturn %s", wrapStr(fmt.Sprintf("cnfrt.ResolveDictSpan(s, %d, o, l)", hdr+4)))
				g.p("\t}")
				g.p("\ti := int(cnfrt.U32S(s[%d:])) + int(o)", hdr)
				g.p("\treturn %s", wrapStr("s[i : i+int(l)]"))
				g.p("}")
			}
			g.p("")
			if sh.kind == kString || sh.kind == kBytes {
				g.p("// %sEquals reports whether the %s field equals v without allocating.", f.getter, f.name)
				g.p("func (c %s) %sEquals(v string) bool {", ct, f.getter)
				g.p("\ts := c.cnfRaw")
				g.p("\tif len(s) < %d {", hdr+4)
				g.p("\t\treturn v == \"\"")
				g.p("\t}")
				g.p("\to, l := cnfrt.U32S(s[%d:]), cnfrt.U32S(s[%d:])", off, off+4)
				g.p("\tif o >= cnfrt.DictSpanBit {")
				g.p("\t\tif l == 0 {")
				g.p("\t\t\treturn v == \"\"")
				g.p("\t\t}")
				g.p("\t\treturn cnfrt.ResolveDictSpan(s, %d, o, l) == v", hdr+4)
				g.p("\t}")
				g.p("\ti := int(cnfrt.U32S(s[%d:])) + int(o)", hdr)
				g.p("\treturn s[i:i+int(l)] == v")
				g.p("}")
				g.p("")
				g.p("// %sLen returns the length of the %s field without allocating.", f.getter, f.name)
				g.p("func (c %s) %sLen() int {", ct, f.getter)
				g.p("\tif c.cnfRaw == \"\" {")
				g.p("\t\treturn 0")
				g.p("\t}")
				g.p("\treturn int(c.cnfU32(%d))", off+4)
				g.p("}")
				g.p("")
				if sh.kind == kBytes {
					g.p("// %sIsNil reports whether the %s field is nil.", f.getter, f.name)
					g.p("func (c %s) %sIsNil() bool {", ct, f.getter)
					g.p("\tif c.cnfRaw == \"\" {")
					g.p("\t\treturn true")
					g.p("\t}")
					g.p("\treturn c.cnfU32(%d) == cnfrt.NilSpanOff", off)
					g.p("}")
					g.p("")
				}
			}
			continue
		}
		if target.kind == kArrayInline && target.n > 64 {
			if sh.kind == kOptional {
				pOff := 1 + flatOffset(n.flat, "PresentBits")
				test := fmt.Sprintf("c.cnfRaw[%d]&(1<<%d) != 0", pOff+f.optBit/8, f.optBit%8)
				g.p("// %s returns the %s field of the root and whether it is set.", f.getter, f.name)
				g.p("func (c %s) %s() (%s, bool) {", ct, f.getter, target.viewType)
				g.p("\tvar out %s", target.viewType)
				g.p("\tif c.cnfRaw == \"\" || !(%s) {", test)
				g.p("\t\treturn out, false")
				g.p("\t}")
				g.genCompactArrayLoop(target, off)
				g.p("\treturn out, true")
				g.p("}")
			} else {
				g.p("// %s returns the %s field of the root without allocating.", f.getter, f.name)
				g.p("func (c %s) %s() %s {", ct, f.getter, sh.viewType)
				g.p("\tvar out %s", sh.viewType)
				g.p("\tif c.cnfRaw == \"\" {")
				g.p("\t\treturn out")
				g.p("\t}")
				g.genCompactArrayLoop(target, off)
				g.p("\treturn out")
				g.p("}")
			}
			g.p("")
			continue
		}
		read := g.compactRead(target, off)
		if read == "" {
			continue
		}
		switch {
		case sh.kind == kOptional:
			pOff := 1 + flatOffset(n.flat, "PresentBits")
			test := fmt.Sprintf("c.cnfRaw[%d]&(1<<%d) != 0", pOff+f.optBit/8, f.optBit%8)
			g.p("// %s returns the %s field of the root and whether it is set.", f.getter, f.name)
			g.p("func (c %s) %s() (%s, bool) {", ct, f.getter, target.viewType)
			g.p("\tif c.cnfRaw == \"\" {")
			g.p("\t\tvar zero %s", target.viewType)
			g.p("\t\treturn zero, false")
			g.p("\t}")
			g.p("\tif !(%s) {", test)
			g.p("\t\tvar zero %s", target.viewType)
			g.p("\t\treturn zero, false")
			g.p("\t}")
			if strings.Count(read, "c.cnfRaw[") > 1 {
				g.p("\t_ = c.cnfRaw[%d] // one bounds check for all reads below", off+target.size-1)
			}
			g.p("\treturn %s, true", read)
			g.p("}")
		default:
			g.p("// %s returns the %s field of the root without allocating.", f.getter, f.name)
			g.p("func (c %s) %s() %s {", ct, f.getter, sh.viewType)
			g.p("\tif c.cnfRaw == \"\" {")
			g.p("\t\tvar zero %s", sh.viewType)
			g.p("\t\treturn zero")
			g.p("\t}")
			if strings.Count(read, "c.cnfRaw[") > 1 {
				g.p("\t_ = c.cnfRaw[%d] // one bounds check for all reads below", off+target.size-1)
			}
			g.p("\treturn %s", read)
			g.p("}")
		}
		g.p("")
	}
}
