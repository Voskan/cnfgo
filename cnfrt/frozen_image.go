package cnfrt

import (
	"fmt"
	"io"
	"reflect"
	"strings"
)

// FrozenMapSchema returns the schema hash of standalone FrozenMap images
// with key type K and value type V. It hashes a structural signature of both
// types: for every type its package path, name, kind and exact size, and
// recursively its fields with their offsets, element types and array
// lengths, plus the schema hash of types implementing SchemaHasher (such as
// generated Compact values). Types that print alike but come from different
// packages, types whose size differs between platforms (such as int on
// 32-bit and 64-bit targets), and Compact values of different schemas
// therefore produce different hashes.
func FrozenMapSchema[K comparable, V any]() uint64 {
	var b strings.Builder
	b.WriteString("cnfgo/frozenmap/v2 ")
	typeSignature(&b, reflect.TypeFor[K](), map[reflect.Type]bool{})
	b.WriteByte(' ')
	typeSignature(&b, reflect.TypeFor[V](), map[reflect.Type]bool{})
	h := HashString(b.String(), 0)
	if h == 0 {
		h = 1
	}
	return h
}

var (
	schemaHasherType = reflect.TypeFor[SchemaHasher]()
	dictUserType     = reflect.TypeFor[DictUser]()
)

// typeSignature appends the structural signature of t to b. Named types are
// expanded once; later occurrences (including recursive ones) refer to them
// by package path and name.
func typeSignature(b *strings.Builder, t reflect.Type, seen map[reflect.Type]bool) {
	fmt.Fprintf(b, "%s.%s:%s/%d", t.PkgPath(), t.Name(), t.Kind(), t.Size())
	if k := t.Kind(); k != reflect.Interface && k != reflect.Pointer && t.Implements(schemaHasherType) {
		fmt.Fprintf(b, "#%016x", reflect.Zero(t).Interface().(SchemaHasher).SchemaHashCNF())
	}
	if t.Name() != "" {
		if seen[t] {
			return
		}
		seen[t] = true
	}
	switch t.Kind() {
	case reflect.Array:
		fmt.Fprintf(b, "[%d]", t.Len())
		typeSignature(b, t.Elem(), seen)
	case reflect.Slice, reflect.Pointer, reflect.Chan:
		b.WriteByte('(')
		typeSignature(b, t.Elem(), seen)
		b.WriteByte(')')
	case reflect.Map:
		b.WriteByte('(')
		typeSignature(b, t.Key(), seen)
		b.WriteByte(',')
		typeSignature(b, t.Elem(), seen)
		b.WriteByte(')')
	case reflect.Struct:
		b.WriteByte('{')
		for i := range t.NumField() {
			f := t.Field(i)
			fmt.Fprintf(b, "%s@%d=", f.Name, f.Offset)
			typeSignature(b, f.Type, seen)
			b.WriteByte(';')
		}
		b.WriteByte('}')
	}
}

// ImageSections returns the .cnf sections holding sec under tag: the control
// bytes (count: number of groups), the slots and the key/value data (count:
// number of entries). The map seed is stored in the image header.
func (sec FrozenSections) ImageSections(tag uint32) []Section {
	return []Section{
		{Kind: SectionFrozenCtrl, ElemAlign: 1, Tag: tag, Count: sec.NumGroups, Data: sec.Ctrl},
		{Kind: SectionFrozenSlots, ElemAlign: 4, Tag: tag, Count: uint32(len(sec.Slots)), Data: ViewSliceLE(sec.Slots)},
		{Kind: SectionFrozenData, ElemAlign: 1, Tag: tag, Count: sec.Len, Data: UnsafeStringBytes(sec.Data)},
	}
}

func (m *FrozenMap[K, V]) imageHeader() Header {
	return Header{SchemaHash: FrozenMapSchema[K, V](), HashSeed: m.seed}
}

// checkPersistable returns ErrDictRequired if V implements DictUser and any
// value of m reports UsesDict: such values reference a process-local Dict
// and would be meaningless in another process.
func (m *FrozenMap[K, V]) checkPersistable() error {
	if !reflect.TypeFor[V]().Implements(dictUserType) {
		return nil
	}
	for _, raw := range m.RawEntries() {
		v, err := m.vc.Decode(raw)
		if err != nil {
			return err
		}
		if any(v).(DictUser).UsesDict() {
			return ErrDictRequired
		}
	}
	return nil
}

// WriteTo writes m as a standalone .cnf image. Such images are opened with
// unsafemmap.OpenFrozenMap or unsafemmap.FrozenMapFromImage. It returns
// ErrDictRequired if any value references a process-local Dict; detach such
// values first.
func (m *FrozenMap[K, V]) WriteTo(w io.Writer) (int64, error) {
	if err := m.checkPersistable(); err != nil {
		return 0, err
	}
	return WriteImage(w, m.imageHeader(), m.Sections().ImageSections(0))
}

// MarshalCNF returns m encoded as a standalone .cnf image. Like WriteTo, it
// returns ErrDictRequired if any value references a process-local Dict.
func (m *FrozenMap[K, V]) MarshalCNF() ([]byte, error) {
	if err := m.checkPersistable(); err != nil {
		return nil, err
	}
	return AppendImage(nil, m.imageHeader(), m.Sections().ImageSections(0))
}
