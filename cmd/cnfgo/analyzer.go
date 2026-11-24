package main

import (
	"fmt"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// shapeKind classifies how a Go type is represented in a snapshot.
type shapeKind int

const (
	kScalar      shapeKind = iota // bool, integers, floats, complex numbers
	kString                       // string: Span32 into the string blob
	kBytes                        // []byte: Span32 into the string blob, nil-aware
	kTime                         // time.Time: cnfrt.FlatTime
	kAddr                         // netip.Addr: cnfrt.FlatAddr
	kPrefix                       // netip.Prefix: cnfrt.FlatPrefix
	kAddrPort                     // netip.AddrPort: cnfrt.FlatAddrPort
	kCodec                        // registered external type: Span32 into the string blob
	kNode                         // struct value: NodeID (never 0)
	kPtrNode                      // pointer to struct: NodeID (0 == nil)
	kOptional                     // pointer to an inline type: value plus presence bit
	kSlice                        // []T: Span32 into a pool
	kArrayInline                  // [N]scalar: inline array
	kArrayPool                    // [N]T for non-scalar T: fixed-length Span32 into a pool
	kMap                          // map[K]V: Span32 into a sorted key/value pool
	kIface                        // closed interface: cnfrt.VariantTag
	kStructKey                    // inline flat comparable struct used as a map key
)

type scalarInfo struct {
	kind    types.BasicKind
	flat    string // flat Go type
	size    int
	isBool  bool
	isFloat bool
	isCplx  bool
	signed  bool
}

type structKeyField struct {
	name     string
	flatName string
	sh       *shape
}

type shape struct {
	kind   shapeKind
	typ    types.Type
	goExpr string // Go type expression in the generated file
	key    string // canonical identity
	id     int

	sc      *scalarInfo
	elem    *shape // slice/array element, optional target, map value
	mkey    *shape // map key
	n       int64  // array length
	node    *node
	iface   *iface
	codec   *codec
	hashed  bool
	sfields []*structKeyField
	sflat   *flatStruct

	flatExpr string
	size     int
	align    int

	stem     string // name stem for generated view types
	viewType string // type returned by readers (single-valued contexts)

	pool    *pool // pool holding the elements of a slice/array, or map entries
	idxPool *pool // hash index pool of hashed maps
	kv      *flatStruct
	opt     *flatStruct // wrapper used when an optional appears in a pool

	refsNodes bool // the flat value can contain node references
}

// maxInlineStructBytes bounds the flat size of non-recursive value structs
// that are automatically inlined into parent records and slice pools. Larger
// structs stay in their own node table so View values stay small.
const maxInlineStructBytes = 128

// inline reports whether s can be the target of an optional pointer field.
func (s *shape) inline() bool {
	switch s.kind {
	case kScalar, kString, kBytes, kTime, kAddr, kPrefix, kAddrPort, kCodec, kArrayInline:
		return true
	}
	return false
}

// isZeroArray reports whether s is a zero-length array [0]T. Zero-length
// arrays occupy 0 bytes in flat records and are omitted from flat structs.
func (s *shape) isZeroArray() bool {
	return (s.kind == kArrayInline || s.kind == kArrayPool) && s.n == 0
}

type field struct {
	name     string // Go field name
	getter   string // View method name
	flatName string
	sh       *shape
	optBit   int // presence bit for kOptional fields, -1 otherwise
	embedded bool
}

type node struct {
	name          string
	typ           types.Type
	goExpr        string
	flatName      string
	canonFlatName string
	viewName      string
	tbl           int
	inlineID      int
	fields        []*field
	flat          *flatStruct
	nopt          int
	scc           int
	selfRef       bool
	isRoot        bool
	usedByPtr     bool
	usedByIface   bool
	inlined       bool
	refsNodes     bool
}

type iface struct {
	name     string
	typ      types.Type
	goExpr   string
	viewName string
	impls    []ifaceImpl
}

type ifaceImpl struct {
	ptr  bool
	node *node
	expr string // Go type expression of the dynamic type
}

type codec struct {
	target      string // import path + "." + name of the encoded type
	adapterPath string
	adapterName string
	typeArgs    []string // adapter type arguments; T denotes the target
	adapter     string   // Go type expression of the adapter (set on first use)
	stem        string
}

// pool is a typed slice in the snapshot holding elements of one shape.
type pool struct {
	idx      int
	key      string
	elem     *shape // nil for hash index pools
	flatExpr string
	size     int
	align    int
	viewName string // slice view type over this pool
	hashIdx  bool
	field    string
}

type flatField struct {
	name  string
	expr  string
	size  int
	align int
	off   int
	pad   bool
}

type flatStruct struct {
	name      string
	canonName string
	fields    []flatField
	size      int
	align     int
}

type schema struct {
	pkg      *types.Package
	prefix   string
	snapName string

	roots    []*node
	compacts []*node
	nodes    []*node
	ifaces   []*iface
	pools    []*pool
	shapes   []*shape
	flats    []*flatStruct
	codecs   []*codec

	byKey      map[string]*shape
	nodeByKey  map[string]*node
	poolByKey  map[string]*pool
	ifaceByKey map[string]*iface
	codecReg   map[string]*codec
	imports    *importSet
	lp         *loadedPackage

	treeDefault    bool
	crossSCC       bool
	hash           uint64
	usesHashMap    bool
	usedNames      map[string]bool
	usedCanonNames map[string]bool
	usedNodeNames  map[string]bool
	errs           []string
	queue          []pendingNode
}

type importSet struct {
	self    *types.Package
	byPath  map[string]string
	byAlias map[string]string
}

func newImportSet(self *types.Package) *importSet {
	return &importSet{self: self, byPath: map[string]string{}, byAlias: map[string]string{}}
}

func (s *importSet) add(path, name string) string {
	if a, ok := s.byPath[path]; ok {
		return a
	}
	alias := name
	for i := 2; ; i++ {
		if _, taken := s.byAlias[alias]; !taken && !reserved[alias] {
			break
		}
		alias = name + strconv.Itoa(i)
	}
	s.byPath[path] = alias
	s.byAlias[alias] = path
	return alias
}

var reserved = map[string]bool{"cnfrt": true, "unsafemmap": true, "binary": true, "math": true,
	"strings": true, "iter": true, "time": true, "netip": true, "io": true, "slices": true,
	"cmp": true, "fmt": true, "reflect": true, "runtime": true, "errors": true, "sync": true}

func init() {
	// Single-letter names are used for receivers and locals in generated code.
	for _, r := range "abcdefghijklmnopqrstuvwxyz" {
		reserved[string(r)] = true
	}
}

func (s *importSet) qualifier(p *types.Package) string {
	if p == s.self {
		return ""
	}
	switch p.Path() {
	case "time":
		return "time"
	case "net/netip":
		return "netip"
	}
	return s.add(p.Path(), p.Name())
}

func (sc *schema) errorf(format string, args ...any) {
	sc.errs = append(sc.errs, fmt.Sprintf(format, args...))
}

func (sc *schema) typeExpr(t types.Type) string {
	return types.TypeString(t, sc.imports.qualifier)
}

func exportName(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func sanitize(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if upper {
				r = unicode.ToUpper(r)
			}
			b.WriteRune(r)
			upper = false
		} else {
			upper = true
		}
	}
	return b.String()
}

func (sc *schema) uniqueName(base string) string {
	name := base
	for i := 2; sc.usedNames[name]; i++ {
		name = base + strconv.Itoa(i)
	}
	sc.usedNames[name] = true
	return name
}

func (sc *schema) uniqueCanonName(base string) string {
	name := base
	for i := 2; sc.usedCanonNames[name]; i++ {
		name = base + strconv.Itoa(i)
	}
	sc.usedCanonNames[name] = true
	return name
}

func (sc *schema) uniqueNodeName(base string) string {
	name := base
	for i := 2; sc.usedNodeNames[name]; i++ {
		name = base + strconv.Itoa(i)
	}
	sc.usedNodeNames[name] = true
	return name
}

var scalarTable = map[types.BasicKind]scalarInfo{
	types.Bool:       {flat: "uint8", size: 1, isBool: true},
	types.Int8:       {flat: "int8", size: 1, signed: true},
	types.Int16:      {flat: "int16", size: 2, signed: true},
	types.Int32:      {flat: "int32", size: 4, signed: true},
	types.Int64:      {flat: "int64", size: 8, signed: true},
	types.Int:        {flat: "int64", size: 8, signed: true},
	types.Uint8:      {flat: "uint8", size: 1},
	types.Uint16:     {flat: "uint16", size: 2},
	types.Uint32:     {flat: "uint32", size: 4},
	types.Uint64:     {flat: "uint64", size: 8},
	types.Uint:       {flat: "uint64", size: 8},
	types.Float32:    {flat: "float32", size: 4, isFloat: true},
	types.Float64:    {flat: "float64", size: 8, isFloat: true},
	types.Complex64:  {flat: "[2]float32", size: 8, isFloat: true, isCplx: true},
	types.Complex128: {flat: "[2]float64", size: 16, isFloat: true, isCplx: true},
}

// flatSizes gives size and alignment (as on 64-bit platforms) of the fixed
// cnfrt flat types.
var flatSizes = map[string][2]int{
	"uint8": {1, 1}, "int8": {1, 1}, "uint16": {2, 2}, "int16": {2, 2},
	"uint32": {4, 4}, "int32": {4, 4}, "float32": {4, 4},
	"uint64": {8, 8}, "int64": {8, 8}, "float64": {8, 8},
	"[2]float32": {8, 4}, "[2]float64": {16, 8},
	"cnfrt.Span32": {8, 4}, "cnfrt.HashSpan": {16, 4}, "cnfrt.VariantTag": {8, 4},
	"cnfrt.FlatTime": {16, 8}, "cnfrt.FlatAddr": {20, 2}, "cnfrt.FlatPrefix": {20, 1},
	"cnfrt.FlatAddrPort": {24, 2},
}

func isNamed(t types.Type, path, name string) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	return n.Obj().Pkg().Path() == path && n.Obj().Name() == name
}

func namedKey(t types.Type) string {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return ""
	}
	return n.Obj().Pkg().Path() + "." + n.Obj().Name()
}

// typeStem returns a name stem for a type: local named types keep their
// name, foreign named types are prefixed with their package name.
func (sc *schema) typeStem(t types.Type) string {
	t = types.Unalias(t)
	if n, ok := t.(*types.Named); ok {
		name := n.Obj().Name()
		if args := n.TypeArgs(); args != nil {
			for i := range args.Len() {
				name += sc.typeStem(args.At(i))
			}
		}
		if n.Obj().Pkg() != nil && n.Obj().Pkg() != sc.pkg {
			return exportName(n.Obj().Pkg().Name()) + exportName(name)
		}
		return exportName(name)
	}
	if b, ok := t.(*types.Basic); ok {
		return exportName(b.Name())
	}
	return sanitize(types.TypeString(t, func(p *types.Package) string { return p.Name() }))
}

func (sc *schema) shapeOf(t types.Type, hashed bool, ctx string) *shape {
	t = types.Unalias(t)
	key := types.TypeString(t, func(p *types.Package) string { return p.Path() })
	if hashed {
		key += "#hash"
	}
	if s, ok := sc.byKey[key]; ok {
		return s
	}
	s := &shape{typ: t, key: key, goExpr: sc.typeExpr(t), hashed: hashed}
	sc.byKey[key] = s
	if !sc.classify(s, t, ctx) {
		return s
	}
	s.id = len(sc.shapes)
	sc.shapes = append(sc.shapes, s)
	return s
}

// mapKeyShape resolves a Go map key type to a shape suitable for inline
// storage in a flatKV record. It supports strings, non-complex scalars,
// netip.Addr, netip.Prefix, netip.AddrPort, fixed arrays of non-complex
// scalars, and flat comparable structs composed of those types.
func (sc *schema) mapKeyShape(t types.Type, ctx string) *shape {
	t = types.Unalias(t)
	if isNamed(t, "net/netip", "Addr") || isNamed(t, "net/netip", "Prefix") || isNamed(t, "net/netip", "AddrPort") {
		return sc.shapeOf(t, false, ctx)
	}
	if _, isStruct := t.Underlying().(*types.Struct); isStruct && !sc.special(t) && sc.codecFor(t) == nil {
		return sc.structKeyShape(t, ctx)
	}
	k := sc.shapeOf(t, false, ctx)
	if k.flatExpr == "" {
		return nil
	}
	switch k.kind {
	case kString:
		return k
	case kScalar:
		if !k.sc.isCplx {
			return k
		}
	case kArrayInline:
		if !k.elem.sc.isCplx {
			return k
		}
	}
	sc.errorf("%s: map key type %s is not supported (use string, bool, integer, float, netip, scalar array, or flat comparable struct keys)", ctx, t)
	return nil
}

func (sc *schema) structKeyShape(t types.Type, ctx string) *shape {
	t = types.Unalias(t)
	key := "skey:" + types.TypeString(t, func(p *types.Package) string { return p.Path() })
	if s, ok := sc.byKey[key]; ok {
		if s.flatExpr == "" {
			return nil
		}
		return s
	}
	st := t.Underlying().(*types.Struct)
	foreign := false
	if named, ok := t.(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Pkg() != sc.pkg {
		foreign = true
	}
	s := &shape{
		kind:     kStructKey,
		typ:      t,
		key:      key,
		goExpr:   sc.typeExpr(t),
		stem:     sc.typeStem(t),
		viewType: sc.typeExpr(t),
	}
	sc.byKey[key] = s
	fs := &flatStruct{
		name:      sc.uniqueName("flatKey" + sc.prefix + s.stem),
		canonName: sc.uniqueCanonName("flatKey" + s.stem),
	}
	flatNames := map[string]bool{}
	for i := range st.NumFields() {
		f := st.Field(i)
		if f.Name() == "_" {
			continue
		}
		if foreign && !f.Exported() {
			sc.errorf("%s: map key struct %s from package %s has unexported field %s", ctx, t, f.Pkg().Path(), f.Name())
			return nil
		}
		fsh := sc.mapKeyShape(f.Type(), ctx+"."+f.Name())
		if fsh == nil {
			return nil
		}
		base := "F" + exportName(f.Name())
		fname := base
		for j := 2; flatNames[fname]; j++ {
			fname = base + strconv.Itoa(j)
		}
		flatNames[fname] = true
		s.sfields = append(s.sfields, &structKeyField{name: f.Name(), flatName: fname, sh: fsh})
		if !fsh.isZeroArray() {
			fs.fields = append(fs.fields, flatField{name: fname, expr: fsh.flatExpr, size: fsh.size, align: fsh.align})
		}
	}
	sc.layout(fs)
	sc.flats = append(sc.flats, fs)
	s.sflat = fs
	s.flatExpr = fs.name
	s.size = fs.size
	s.align = fs.align
	s.id = len(sc.shapes)
	sc.shapes = append(sc.shapes, s)
	return s
}

func (sc *schema) classify(s *shape, t types.Type, ctx string) bool {
	switch {
	case isNamed(t, "time", "Time"):
		s.kind, s.flatExpr, s.stem, s.viewType = kTime, "cnfrt.FlatTime", "Time", "time.Time"
		return sc.finishFixed(s)
	case isNamed(t, "net/netip", "Addr"):
		s.kind, s.flatExpr, s.stem, s.viewType = kAddr, "cnfrt.FlatAddr", "Addr", "netip.Addr"
		return sc.finishFixed(s)
	case isNamed(t, "net/netip", "Prefix"):
		s.kind, s.flatExpr, s.stem, s.viewType = kPrefix, "cnfrt.FlatPrefix", "Prefix", "netip.Prefix"
		return sc.finishFixed(s)
	case isNamed(t, "net/netip", "AddrPort"):
		s.kind, s.flatExpr, s.stem, s.viewType = kAddrPort, "cnfrt.FlatAddrPort", "AddrPort", "netip.AddrPort"
		return sc.finishFixed(s)
	}
	if c := sc.codecFor(t); c != nil {
		s.kind, s.codec, s.flatExpr, s.stem = kCodec, c, "cnfrt.Span32", c.stem
		s.viewType = "cnfrt.CodecValue[" + s.goExpr + ", " + c.adapter + "]"
		return sc.finishFixed(s)
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		info, ok := scalarTable[u.Kind()]
		if u.Kind() == types.String {
			s.kind, s.flatExpr, s.stem, s.viewType = kString, "cnfrt.Span32", "String", "string"
			if _, named := t.(*types.Named); named {
				s.stem, s.viewType = sc.typeStem(t), s.goExpr
			}
			return sc.finishFixed(s)
		}
		if !ok {
			sc.errorf("%s: unsupported basic type %s", ctx, t)
			return false
		}
		info.kind = u.Kind()
		s.kind, s.sc, s.flatExpr, s.stem, s.viewType = kScalar, &info, info.flat, sc.typeStem(t), s.goExpr
		return sc.finishFixed(s)
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			if _, named := types.Unalias(u.Elem()).(*types.Named); !named {
				s.kind, s.flatExpr, s.stem, s.viewType = kBytes, "cnfrt.Span32", "Bytes", "string"
				if _, namedSlice := t.(*types.Named); namedSlice {
					s.stem = sc.typeStem(t)
				}
				return sc.finishFixed(s)
			}
		}
		s.kind = kSlice
		s.elem = sc.shapeOf(u.Elem(), false, ctx+"[]")
		s.flatExpr = "cnfrt.Span32"
		sc.finishFixed(s)
		if s.elem.flatExpr == "" {
			return false
		}
		s.pool = sc.poolFor(s.elem)
		s.stem = s.elem.stem + "Slice"
		s.viewType = s.pool.viewName
		s.refsNodes = s.elem.refsNodes
		return true
	case *types.Array:
		elem := sc.shapeOf(u.Elem(), false, ctx+"[N]")
		if elem.flatExpr == "" {
			return false
		}
		s.elem, s.n = elem, u.Len()
		if elem.kind == kScalar {
			s.kind = kArrayInline
			s.flatExpr = fmt.Sprintf("[%d]%s", u.Len(), elem.flatExpr)
			s.size, s.align = int(u.Len())*elem.size, elem.align
			if u.Len() == 0 {
				s.size, s.align = 0, 1
			}
			s.stem = elem.stem + "Array" + strconv.FormatInt(u.Len(), 10)
			s.viewType = s.goExpr
			return true
		}
		s.kind = kArrayPool
		s.pool = sc.poolFor(elem)
		s.stem = elem.stem + "Array" + strconv.FormatInt(u.Len(), 10)
		s.viewType = s.pool.viewName
		if u.Len() == 0 {
			s.flatExpr = "[0]byte"
			s.size, s.align = 0, 1
			s.refsNodes = false
			return true
		}
		s.flatExpr = "cnfrt.Span32"
		sc.finishFixed(s)
		s.refsNodes = elem.refsNodes
		return true
	case *types.Map:
		k := sc.mapKeyShape(u.Key(), ctx+" map key")
		if k == nil || k.flatExpr == "" {
			return false
		}
		v := sc.shapeOf(u.Elem(), false, ctx+" map value")
		if v.flatExpr == "" {
			return false
		}
		s.kind, s.mkey, s.elem = kMap, k, v
		s.flatExpr = "cnfrt.Span32"
		if s.hashed {
			s.flatExpr = "cnfrt.HashSpan"
			sc.usesHashMap = true
		}
		sc.finishFixed(s)
		s.stem = k.stem + v.stem + "Map"
		if s.hashed {
			s.stem = k.stem + v.stem + "HashMap"
		}
		s.viewType = sc.uniqueName(sc.prefix + s.stem + "View")
		s.kv = &flatStruct{
			name:      sc.uniqueName("flatKV" + sc.prefix + s.stem),
			canonName: sc.uniqueCanonName("flatKV" + s.stem),
		}
		if !k.isZeroArray() {
			s.kv.fields = append(s.kv.fields, flatField{name: "K", expr: k.flatExpr, size: k.size, align: k.align})
		}
		if !v.isZeroArray() {
			s.kv.fields = append(s.kv.fields, sc.poolField("V", v))
		}
		sc.layout(s.kv)
		sc.flats = append(sc.flats, s.kv)
		s.pool = sc.addPool("kv:"+key(s), nil, s.kv.name, s.kv.size, s.kv.align, "")
		s.pool.elem = s
		if s.hashed {
			s.idxPool = sc.addPool("hidx:"+key(s), nil, "uint32", 4, 4, "")
			s.idxPool.hashIdx = true
		}
		s.refsNodes = v.refsNodes
		return true
	case *types.Pointer:
		elemT := types.Unalias(u.Elem())
		if _, isStruct := elemT.Underlying().(*types.Struct); isStruct && !sc.special(elemT) && sc.codecFor(elemT) == nil {
			n := sc.nodeOf(elemT, ctx)
			if n == nil {
				return false
			}
			n.usedByPtr = true
			s.kind, s.node, s.flatExpr, s.stem, s.viewType = kPtrNode, n, "uint32", n.name, n.viewName
			s.refsNodes = true
			return sc.finishFixed(s)
		}
		target := sc.shapeOf(elemT, false, ctx+"*")
		if target.flatExpr == "" {
			return false
		}
		if !target.inline() {
			sc.errorf("%s: pointer to %s is not supported (pointers may target structs or inline values such as scalars, strings and time.Time)", ctx, elemT)
			return false
		}
		s.kind, s.elem = kOptional, target
		s.flatExpr, s.size, s.align = target.flatExpr, target.size, target.align
		s.stem = "Opt" + target.stem
		s.viewType = "cnfrt.Optional[" + target.viewType + "]"
		return true
	case *types.Struct:
		n := sc.nodeOf(t, ctx)
		if n == nil {
			return false
		}
		s.kind, s.node, s.flatExpr, s.stem, s.viewType = kNode, n, "uint32", n.name, n.viewName
		s.refsNodes = true
		return sc.finishFixed(s)
	case *types.Interface:
		ifc := sc.ifaceOf(t, ctx)
		if ifc == nil {
			return false
		}
		s.kind, s.iface, s.flatExpr, s.stem, s.viewType = kIface, ifc, "cnfrt.VariantTag", ifc.name, ifc.viewName
		s.refsNodes = true
		return sc.finishFixed(s)
	}
	sc.errorf("%s: type %s is not supported (channels, functions and pointers to pointers cannot be frozen; register a -codec for opaque types)", ctx, t)
	return false
}

func key(s *shape) string { return s.key }

func (sc *schema) special(t types.Type) bool {
	return isNamed(t, "time", "Time") || isNamed(t, "net/netip", "Addr") ||
		isNamed(t, "net/netip", "Prefix") || isNamed(t, "net/netip", "AddrPort")
}

func (sc *schema) finishFixed(s *shape) bool {
	sz, ok := flatSizes[s.flatExpr]
	if !ok {
		panic("unknown flat type " + s.flatExpr)
	}
	s.size, s.align = sz[0], sz[1]
	return true
}

// poolField returns the flat field used to store a value of shape v inside
// a pool element (map value or slice element).
func (sc *schema) poolField(name string, v *shape) flatField {
	if v.kind == kOptional {
		w := sc.optWrapper(v)
		return flatField{name: name, expr: w.name, size: w.size, align: w.align}
	}
	if v.isZeroArray() {
		return flatField{name: name, expr: "[1]byte", size: 1, align: 1}
	}
	return flatField{name: name, expr: v.flatExpr, size: v.size, align: v.align}
}

func (sc *schema) optWrapper(v *shape) *flatStruct {
	if v.opt != nil {
		return v.opt
	}
	w := &flatStruct{
		name:      sc.uniqueName("flat" + sc.prefix + v.stem),
		canonName: sc.uniqueCanonName("flat" + v.stem),
	}
	if !v.elem.isZeroArray() {
		w.fields = append(w.fields, flatField{name: "V", expr: v.elem.flatExpr, size: v.elem.size, align: v.elem.align})
	}
	w.fields = append(w.fields, flatField{name: "P", expr: "uint8", size: 1, align: 1})
	sc.layout(w)
	sc.flats = append(sc.flats, w)
	v.opt = w
	return w
}

func (sc *schema) poolKeyFor(elem *shape) string {
	switch elem.kind {
	case kNode, kPtrNode:
		return "id:" + elem.node.name
	}
	return "elem:" + elem.key
}

func (sc *schema) poolFor(elem *shape) *pool {
	pk := sc.poolKeyFor(elem)
	if p, ok := sc.poolByKey[pk]; ok {
		return p
	}
	f := sc.poolField("E", elem)
	view := sc.uniqueName(sc.prefix + elem.stem + "SliceView")
	p := sc.addPool(pk, elem, f.expr, f.size, f.align, view)
	return p
}

func (sc *schema) addPool(pk string, elem *shape, flat string, size, align int, view string) *pool {
	p := &pool{idx: len(sc.pools), key: pk, elem: elem, flatExpr: flat, size: size, align: align, viewName: view}
	p.field = "pool" + strconv.Itoa(p.idx)
	sc.pools = append(sc.pools, p)
	sc.poolByKey[pk] = p
	return p
}

func (sc *schema) codecFor(t types.Type) *codec {
	k := namedKey(t)
	if k == "" {
		return nil
	}
	c, ok := sc.codecReg[k]
	if !ok {
		return nil
	}
	if c.adapter == "" {
		c.adapter = sc.adapterExpr(c, sc.typeExpr(t))
		sc.codecs = append(sc.codecs, c)
	}
	return c
}

func (sc *schema) nodeOf(t types.Type, ctx string) *node {
	t = types.Unalias(t)
	k := types.TypeString(t, func(p *types.Package) string { return p.Path() })
	if n, ok := sc.nodeByKey[k]; ok {
		return n
	}
	named, ok := t.(*types.Named)
	if !ok {
		sc.errorf("%s: anonymous struct type %s is not supported; declare a named type", ctx, t)
		return nil
	}
	st := named.Underlying().(*types.Struct)
	foreign := named.Obj().Pkg() != sc.pkg
	n := &node{typ: t, goExpr: sc.typeExpr(t), tbl: len(sc.nodes)}
	n.name = sc.uniqueNodeName(sc.typeStem(t))
	n.flatName = sc.uniqueName("flat" + sc.prefix + n.name)
	n.canonFlatName = sc.uniqueCanonName("flat" + n.name)
	n.viewName = sc.uniqueName(sc.prefix + n.name + "View")
	sc.nodeByKey[k] = n
	sc.nodes = append(sc.nodes, n)
	sc.queue = append(sc.queue, pendingNode{n: n, named: named, st: st, foreign: foreign, ctx: ctx})
	return n
}

type pendingNode struct {
	n       *node
	named   *types.Named
	st      *types.Struct
	foreign bool
	ctx     string
}

// drain analyzes the fields of queued node types. Fields are analyzed after
// a node is registered, so recursive types resolve to complete shapes.
func (sc *schema) drain() {
	for len(sc.queue) > 0 {
		p := sc.queue[0]
		sc.queue = sc.queue[1:]
		sc.nodeFields(p.n, p.named, p.st, p.foreign, p.ctx)
	}
}

func (sc *schema) nodeFields(n *node, named *types.Named, st *types.Struct, foreign bool, ctx string) {
	getters := map[string]string{"IsValid": "(built-in)", "NodeID": "(built-in)", "Snapshot": "(built-in)", "Unfreeze": "(built-in)"}
	flatNames := map[string]bool{}
	for i := range st.NumFields() {
		f := st.Field(i)
		tag := reflect.StructTag(st.Tag(i)).Get("cnf")
		opts := strings.Split(tag, ",")
		if opts[0] == "-" {
			continue
		}
		if f.Name() == "_" {
			continue
		}
		fctx := fmt.Sprintf("%s.%s", named.Obj().Name(), f.Name())
		if opts[0] != "" {
			if opts[0] == "hash" {
				sc.errorf("%s: invalid cnf tag %q: use `cnf:\",hash\"` with a leading comma", fctx, tag)
			} else {
				sc.errorf("%s: unknown cnf tag option %q", fctx, opts[0])
			}
			continue
		}
		if foreign && !f.Exported() {
			sc.errorf("%s: type %s from package %s has unexported fields; register a -codec for it", ctx, named.Obj().Name(), named.Obj().Pkg().Path())
			return
		}
		hashed := false
		for _, o := range opts[1:] {
			switch o {
			case "hash":
				hashed = true
			case "":
			default:
				sc.errorf("%s: unknown cnf tag option %q", fctx, o)
			}
		}
		if hashed {
			if _, isMap := f.Type().Underlying().(*types.Map); !isMap {
				sc.errorf("%s: the hash option applies only to map fields", fctx)
				hashed = false
			}
		}
		fs := sc.shapeOf(f.Type(), hashed, fctx)
		if fs.flatExpr == "" {
			continue
		}
		fl := &field{name: f.Name(), getter: f.Name(), sh: fs, optBit: -1, embedded: f.Embedded()}
		base := "F" + exportName(f.Name())
		fl.flatName = base
		for j := 2; flatNames[fl.flatName]; j++ {
			fl.flatName = base + strconv.Itoa(j)
		}
		flatNames[fl.flatName] = true
		if fs.kind == kOptional {
			fl.optBit = n.nopt
			n.nopt++
		}
		if reservedInternal(fl.getter) {
			sc.errorf("%s: field name %s is reserved: generated code uses unexported identifiers with the cnf prefix", fctx, f.Name())
			continue
		}
		for _, m := range methodNames(fl) {
			if prev, dup := getters[m]; dup {
				sc.errorf("%s: generated method %s of %s collides with field %s", fctx, m, n.viewName, prev)
			}
			getters[m] = f.Name()
		}
		n.fields = append(n.fields, fl)
	}
}

// reservedInternal reports whether name has the form cnf or cnfX with an
// upper-case X, which generated code reserves for its unexported fields and
// helper methods (cnfSnap, cnfRaw, cnfStr, ...).
func reservedInternal(name string) bool {
	rest, ok := strings.CutPrefix(name, "cnf")
	return ok && (rest == "" || rest[0] >= 'A' && rest[0] <= 'Z')
}

// checkCompactNames reports root fields of Compact types whose generated
// getters would collide with the methods of the Compact type.
func (sc *schema) checkCompactNames() {
	for _, n := range sc.compacts {
		for _, f := range n.fields {
			names := []string{f.getter}
			if k := f.sh.kind; k == kString || k == kBytes {
				names = append(names, f.getter+"Equals", f.getter+"Len")
				if k == kBytes {
					names = append(names, f.getter+"IsNil")
				}
			}
			for _, m := range names {
				if compactReserved[m] {
					sc.errorf("%s.%s: generated getter %s collides with a method of Compact%s%s", n.name, f.name, m, sc.prefix, n.name)
				}
			}
		}
	}
}

// methodNames lists the View methods generated for a field.
func methodNames(f *field) []string {
	names := []string{f.getter}
	switch f.sh.kind {
	case kString, kBytes:
		names = append(names, f.getter+"Equals", "Append"+exportName(f.getter), "With"+exportName(f.getter), f.getter+"Len")
		if f.sh.kind == kBytes {
			names = append(names, f.getter+"IsNil")
		}
	case kCodec:
		names = append(names, f.getter+"Raw")
	}
	return names
}

func (sc *schema) ifaceOf(t types.Type, ctx string) *iface {
	k := types.TypeString(t, func(p *types.Package) string { return p.Path() })
	if i, ok := sc.ifaceByKey[k]; ok {
		return i
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() != sc.pkg {
		sc.errorf("%s: interface type %s is not supported; declare a closed interface in this package with a //cnf:implementations=A,*B directive", ctx, t)
		return nil
	}
	var list string
	for _, line := range sc.lp.typeDoc(named.Obj().Name()) {
		line = strings.TrimSpace(strings.TrimPrefix(line, "//"))
		if v, ok := strings.CutPrefix(line, "cnf:implementations="); ok {
			list = v
		}
	}
	if list == "" {
		sc.errorf("%s: interface %s has no //cnf:implementations directive", ctx, named.Obj().Name())
		return nil
	}
	ifc := &iface{name: exportName(named.Obj().Name()), typ: t, goExpr: sc.typeExpr(t)}
	ifc.viewName = sc.uniqueName(sc.prefix + ifc.name + "View")
	sc.ifaceByKey[k] = ifc
	sc.ifaces = append(sc.ifaces, ifc)
	itype := named.Underlying().(*types.Interface)
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		ptr := strings.HasPrefix(item, "*")
		obj := sc.pkg.Scope().Lookup(strings.TrimPrefix(item, "*"))
		tn, ok := obj.(*types.TypeName)
		if !ok {
			sc.errorf("%s: implementation %q of %s is not a type in this package", ctx, item, ifc.name)
			continue
		}
		var dyn types.Type = tn.Type()
		if ptr {
			dyn = types.NewPointer(tn.Type())
		}
		if !types.Implements(dyn, itype) {
			sc.errorf("%s: %s does not implement %s", ctx, item, ifc.name)
			continue
		}
		if _, isStruct := tn.Type().Underlying().(*types.Struct); !isStruct {
			sc.errorf("%s: implementation %s of %s must be a struct type", ctx, item, ifc.name)
			continue
		}
		n := sc.nodeOf(tn.Type(), ctx)
		if n == nil {
			continue
		}
		n.usedByIface = true
		ifc.impls = append(ifc.impls, ifaceImpl{ptr: ptr, node: n, expr: sc.typeExpr(dyn)})
	}
	if len(ifc.impls) > 1<<16-2 {
		sc.errorf("%s: too many implementations", ctx)
	}
	return ifc
}

// layout orders the fields of a flat struct by decreasing alignment (as on
// 64-bit platforms) and inserts explicit padding so that offsets and size
// are identical on 32-bit and 64-bit platforms and the in-memory layout
// equals the little-endian wire layout.
func (sc *schema) layout(fs *flatStruct) {
	fields := append([]flatField(nil), fs.fields...)
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].align > fields[j].align })
	off, maxAlign := 0, 1
	var out []flatField
	for _, f := range fields {
		if f.align > maxAlign {
			maxAlign = f.align
		}
		if rem := off % f.align; rem != 0 {
			pad := f.align - rem
			out = append(out, flatField{name: "_", expr: fmt.Sprintf("[%d]byte", pad), size: pad, align: 1, off: off, pad: true})
			off += pad
		}
		f.off = off
		out = append(out, f)
		off += f.size
	}
	if rem := off % maxAlign; rem != 0 {
		pad := maxAlign - rem
		out = append(out, flatField{name: "_", expr: fmt.Sprintf("[%d]byte", pad), size: pad, align: 1, off: off, pad: true})
		off += pad
	}
	if off == 0 {
		out = append(out, flatField{name: "_", expr: "[1]byte", size: 1, align: 1, pad: true})
		off = 1
	}
	fs.fields, fs.size, fs.align = out, off, maxAlign
}

// presenceType returns the type of the presence bitmap for n optional
// fields.
func presenceType(n int) (string, int, int) {
	switch {
	case n <= 8:
		return "uint8", 1, 1
	case n <= 16:
		return "uint16", 2, 2
	case n <= 32:
		return "uint32", 4, 4
	case n <= 64:
		return "uint64", 8, 8
	}
	w := (n + 63) / 64
	return fmt.Sprintf("[%d]uint64", w), 8 * w, 8
}

func (sc *schema) tableNodes() []*node {
	var out []*node
	for _, n := range sc.nodes {
		if !n.inlined {
			out = append(out, n)
		}
	}
	return out
}

func (sc *schema) inlineNodes() []*node {
	var out []*node
	for _, n := range sc.nodes {
		if n.inlined {
			out = append(out, n)
		}
	}
	return out
}

func (sc *schema) layoutNode(n *node) {
	fs := &flatStruct{name: n.flatName, canonName: n.canonFlatName}
	for _, f := range n.fields {
		if f.sh.isZeroArray() || (f.sh.kind == kOptional && f.sh.elem.isZeroArray()) {
			continue
		}
		fs.fields = append(fs.fields, flatField{name: f.flatName, expr: f.sh.flatExpr, size: f.sh.size, align: f.sh.align})
	}
	if n.nopt > 0 {
		expr, size, align := presenceType(n.nopt)
		fs.fields = append(fs.fields, flatField{name: "PresentBits", expr: expr, size: size, align: align})
	}
	sc.layout(fs)
	n.flat = fs
}

// finalize computes strongly connected components, identifies non-recursive
// value-only structs up to maxInlineStructBytes to inline directly into their
// parent records and slice pools, lays out all flat structs in bottom-up
// dependency order, and recomputes transitive node-reference flags.
func (sc *schema) finalize() {
	sccSizes := sc.computeSCC()

	// Candidate nodes for auto-inlining: non-root structs used exclusively by
	// value and outside any cycle. Because candidates have SCC size 1 and are
	// not self-referential, the subgraph they induce is acyclic and can be
	// laid out bottom-up to test each candidate against maxInlineStructBytes.
	cand := make(map[*node]bool, len(sc.nodes))
	for _, n := range sc.nodes {
		if !n.isRoot && !n.usedByPtr && !n.usedByIface && !n.selfRef && sccSizes[n.scc] == 1 {
			cand[n] = true
		}
	}

	var orderedInlined []*node
	visited := map[*node]bool{}
	var visitCand func(n *node)
	visitCand = func(n *node) {
		if visited[n] {
			return
		}
		visited[n] = true
		for _, w := range sc.nodeEdges(n) {
			if cand[w] {
				visitCand(w)
			}
		}
		sc.layoutNode(n)
		if n.flat.size <= maxInlineStructBytes {
			n.inlined = true
			for _, sh := range sc.shapes {
				if sh.kind == kNode && sh.node == n {
					sh.flatExpr = n.flatName
					sh.size = n.flat.size
					sh.align = n.flat.align
				}
			}
			orderedInlined = append(orderedInlined, n)
		}
	}
	for _, n := range sc.nodes {
		if cand[n] {
			visitCand(n)
		}
	}

	tblIdx, inlineIdx := 0, 0
	for _, n := range sc.nodes {
		if n.inlined {
			n.tbl = -1
			n.inlineID = inlineIdx
			inlineIdx++
		} else {
			n.tbl = tblIdx
			n.inlineID = -1
			tblIdx++
		}
	}

	// Update maps and pools whose value/element is an inlined struct.
	for _, sh := range sc.shapes {
		if sh.kind == kMap && sh.elem.kind == kNode && sh.elem.node.inlined {
			sh.kv.fields = nil
			if !sh.mkey.isZeroArray() {
				sh.kv.fields = append(sh.kv.fields, flatField{name: "K", expr: sh.mkey.flatExpr, size: sh.mkey.size, align: sh.mkey.align})
			}
			sh.kv.fields = append(sh.kv.fields, sc.poolField("V", sh.elem))
			sc.layout(sh.kv)
			sh.pool.size = sh.kv.size
			sh.pool.align = sh.kv.align
		}
	}
	for _, p := range sc.pools {
		if p.elem != nil && p.elem.kind == kNode && p.elem.node.inlined {
			p.flatExpr = p.elem.flatExpr
			p.size = p.elem.size
			p.align = p.elem.align
		}
	}

	// Lay out table nodes (they may now embed inlined flat structs directly).
	for _, n := range orderedInlined {
		sc.flats = append(sc.flats, n.flat)
	}
	for _, n := range sc.nodes {
		if !n.inlined {
			sc.layoutNode(n)
			sc.flats = append(sc.flats, n.flat)
		}
	}

	// Recompute refsNodes bottom-up now that value-only structs may be inlined.
	for _, sh := range sc.shapes {
		switch sh.kind {
		case kPtrNode, kIface:
			sh.refsNodes = true
		case kNode:
			sh.refsNodes = !sh.node.inlined
		default:
			sh.refsNodes = false
		}
	}
	for changed := true; changed; {
		changed = false
		for _, n := range orderedInlined {
			if n.refsNodes {
				continue
			}
			for _, f := range n.fields {
				if f.sh.refsNodes {
					n.refsNodes = true
					changed = true
					break
				}
			}
		}
		for _, sh := range sc.shapes {
			if sh.refsNodes {
				continue
			}
			switch sh.kind {
			case kNode:
				if sh.node.inlined && sh.node.refsNodes {
					sh.refsNodes = true
					changed = true
				}
			case kSlice, kArrayPool, kMap:
				if sh.elem.refsNodes {
					sh.refsNodes = true
					changed = true
				}
			}
		}
	}
}

func (sc *schema) nodeEdges(n *node) []*node {
	seen := map[*node]bool{}
	var out []*node
	var walk func(s *shape)
	visited := map[*shape]bool{}
	walk = func(s *shape) {
		if s == nil || visited[s] {
			return
		}
		visited[s] = true
		switch s.kind {
		case kNode, kPtrNode:
			if !seen[s.node] {
				seen[s.node] = true
				out = append(out, s.node)
			}
		case kIface:
			for _, im := range s.iface.impls {
				if !seen[im.node] {
					seen[im.node] = true
					out = append(out, im.node)
				}
			}
		}
		walk(s.elem)
		walk(s.mkey)
	}
	for _, f := range n.fields {
		walk(f.sh)
	}
	return out
}

func (sc *schema) computeSCC() map[int]int {
	index := map[*node]int{}
	low := map[*node]int{}
	onStack := map[*node]bool{}
	sccSizes := map[int]int{}
	var stack []*node
	counter, sccID := 0, 0
	var strong func(v *node)
	strong = func(v *node) {
		index[v], low[v] = counter, counter
		counter++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range sc.nodeEdges(v) {
			if w == v {
				v.selfRef = true
			}
			if _, ok := index[w]; !ok {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			size := 0
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				w.scc = sccID
				size++
				if w == v {
					break
				}
			}
			sccSizes[sccID] = size
			if size > 1 {
				sc.crossSCC = true
			}
			sccID++
		}
	}
	for _, n := range sc.nodes {
		if _, ok := index[n]; !ok {
			strong(n)
		}
	}
	return sccSizes
}
