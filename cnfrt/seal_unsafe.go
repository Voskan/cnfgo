package cnfrt

import (
	"fmt"
	"reflect"
	"unsafe"
)

// UnsafeSealBytes returns a string that shares b's backing array without
// copying. The caller must guarantee that b is never modified afterwards;
// generated Freeze functions use it exactly once, on a buffer they own, at
// the end of the second pass.
func UnsafeSealBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// UnsafeStringBytes returns a byte slice that aliases the bytes of s. The
// result must be treated as read-only.
func UnsafeStringBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// Addr returns the address of p as an integer. It is used as the identity
// of a source object while it is kept alive by the caller (Go's collector
// does not move heap objects). The result must never be converted back to a
// pointer.
func Addr[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

var escapeSink struct {
	enabled bool
	v       any
}

// Escape forces x to be heap allocated by the caller. Generated Freeze
// functions pass their root to Escape so that the source graph cannot live
// on a goroutine stack, whose addresses could change during traversal.
func Escape(x any) {
	if escapeSink.enabled {
		escapeSink.v = x
	}
}

// AssertNoPointers returns an error if T contains Go pointers at any depth,
// that is, if a []T would be allocated in a scanned span class. Generated
// code uses it in tests and init-time checks for every flat type.
func AssertNoPointers[T any]() error {
	return CheckNoPointers(reflect.TypeFor[T]())
}

// CheckNoPointers is the reflect.Type form of AssertNoPointers.
func CheckNoPointers(t reflect.Type) error {
	if path, ok := pointerPath(t, t.String()); ok {
		return fmt.Errorf("cnfrt: %s contains a pointer at %s", t, path)
	}
	return nil
}

func pointerPath(t reflect.Type, path string) (string, bool) {
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.String, reflect.Slice,
		reflect.Map, reflect.Chan, reflect.Func, reflect.Interface:
		return path + " (" + t.Kind().String() + ")", true
	case reflect.Array:
		if t.Len() == 0 {
			return "", false
		}
		return pointerPath(t.Elem(), path+"[i]")
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if p, ok := pointerPath(f.Type, path+"."+f.Name); ok {
				return p, true
			}
		}
	}
	return "", false
}

// AlignedCopy returns a copy of b whose first byte is 64-byte aligned. It is
// used by VerifyUntrusted to move an image into the Go heap with the
// alignment required for zero-copy table casts.
func AlignedCopy(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	words := make([]uint64, (len(b)+63+7)/8)
	base := unsafe.Pointer(unsafe.SliceData(words))
	skip := (64 - int(uintptr(base)&63)) & 63
	all := unsafe.Slice((*byte)(base), len(words)*8)
	out := all[skip : skip+len(b) : skip+len(b)]
	copy(out, b)
	return out
}

// CapBytes returns the size in bytes of the backing array of s.
func CapBytes[T any](s []T) int {
	var zero T
	return cap(s) * int(unsafe.Sizeof(zero))
}
