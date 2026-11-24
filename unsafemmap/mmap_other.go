//go:build !unix

package unsafemmap

import (
	"fmt"
	"os"

	"github.com/Voskan/cnfgo/cnfrt"
)

// Open reads the file at path into an aligned heap buffer. Platforms
// without mmap support get the same API with copy semantics.
func Open(path string) (*Mapping, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrEmptyFile, path)
	}
	return &Mapping{data: cnfrt.AlignedCopy(b)}, nil
}

func unmap([]byte) error { return nil }
