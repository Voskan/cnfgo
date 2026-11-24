//go:build !cnfgo_portable && (386 || amd64 || arm || arm64 || loong64 || mips64le || mipsle || ppc64le || riscv64 || wasm)

package cnfrt

import "unsafe"

// NativeLittleEndian reports whether in-memory flat tables share the
// little-endian .cnf wire layout, which enables zero-copy encoding and
// decoding.
const NativeLittleEndian = true

// AppendSliceLE appends the little-endian wire encoding of s to dst. On
// little-endian platforms this is a single memory copy.
func AppendSliceLE[T any](dst []byte, s []T) []byte {
	if len(s) == 0 {
		return dst
	}
	var zero T
	n := len(s) * int(unsafe.Sizeof(zero))
	return append(dst, unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(s))), n)...)
}

// SliceSize returns the wire size in bytes of s.
func SliceSize[T any](s []T) int {
	var zero T
	return len(s) * int(unsafe.Sizeof(zero))
}

// ViewSliceLE returns the little-endian wire encoding of s. On
// little-endian platforms the result aliases s and must not be modified.
func ViewSliceLE[T any](s []T) []byte {
	if len(s) == 0 {
		return nil
	}
	var zero T
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(s))), len(s)*int(unsafe.Sizeof(zero)))
}

// DecodeSliceLE copies the little-endian wire encoding src into dst.
// len(src) must equal SliceSize(dst).
func DecodeSliceLE[T any](dst []T, src []byte) error {
	if len(dst) == 0 {
		if len(src) != 0 {
			return ErrBoundsViolation
		}
		return nil
	}
	var zero T
	n := len(dst) * int(unsafe.Sizeof(zero))
	if len(src) != n {
		return ErrBoundsViolation
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(dst))), n), src)
	return nil
}
