//go:build !cnfgo_portable && (386 || amd64 || arm || arm64 || loong64 || mips64le || mipsle || ppc64le || riscv64 || wasm)

package unsafemmap

import "unsafe"

func castLE[T any](b []byte, n int) ([]T, error) {
	var zero T
	p := unsafe.Pointer(unsafe.SliceData(b))
	if uintptr(p)%unsafe.Alignof(zero) == 0 {
		return unsafe.Slice((*T)(p), n), nil
	}
	out := make([]T, n)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(out))), len(b)), b)
	return out, nil
}
