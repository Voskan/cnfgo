//go:build cnfgo_portable || !(386 || amd64 || arm || arm64 || loong64 || mips64le || mipsle || ppc64le || riscv64 || wasm)

package unsafemmap

import (
	"encoding/binary"
	"fmt"

	"github.com/Voskan/cnfgo/cnfrt"
)

func castLE[T any](b []byte, n int) ([]T, error) {
	out := make([]T, n)
	if _, err := binary.Decode(b, binary.LittleEndian, out); err != nil {
		return nil, fmt.Errorf("%w: %v", cnfrt.ErrBoundsViolation, err)
	}
	return out, nil
}
