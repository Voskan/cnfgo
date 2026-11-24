//go:build cnfgo_portable || !(386 || amd64 || arm || arm64 || loong64 || mips64le || mipsle || ppc64le || riscv64 || wasm)

package cnfrt

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// NativeLittleEndian reports whether in-memory flat tables share the
// little-endian .cnf wire layout, which enables zero-copy encoding and
// decoding.
const NativeLittleEndian = false

// AppendSliceLE appends the little-endian wire encoding of s to dst using
// encoding/binary, which is portable to big-endian platforms. Flat types
// are generated with explicit padding, so their wire layout equals their
// little-endian memory layout.
func AppendSliceLE[T any](dst []byte, s []T) []byte {
	if len(s) == 0 {
		return dst
	}
	out, err := binary.Append(dst, binary.LittleEndian, s)
	if err != nil {
		panic("cnfrt: AppendSliceLE: " + err.Error())
	}
	return out
}

// SliceSize returns the wire size in bytes of s.
func SliceSize[T any](s []T) int {
	var zero T
	return len(s) * int(unsafe.Sizeof(zero))
}

// ViewSliceLE returns the little-endian wire encoding of s in a new slice.
func ViewSliceLE[T any](s []T) []byte { return AppendSliceLE(nil, s) }

// DecodeSliceLE decodes the little-endian wire encoding src into dst using
// encoding/binary.
func DecodeSliceLE[T any](dst []T, src []byte) error {
	if len(dst) == 0 {
		if len(src) != 0 {
			return ErrBoundsViolation
		}
		return nil
	}
	n, err := binary.Decode(src, binary.LittleEndian, dst)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBoundsViolation, err)
	}
	if n != len(src) {
		return ErrBoundsViolation
	}
	return nil
}
