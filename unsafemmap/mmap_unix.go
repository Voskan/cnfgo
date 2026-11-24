//go:build unix

package unsafemmap

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

// Open maps the file at path into memory with PROT_READ and MAP_PRIVATE.
func Open(path string) (*Mapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	switch {
	case size == 0:
		return nil, fmt.Errorf("%w: %s", ErrEmptyFile, path)
	case size < 0 || uint64(size) > math.MaxInt:
		return nil, fmt.Errorf("unsafemmap: %s: file too large to map", path)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil, &os.PathError{Op: "mmap", Path: path, Err: err}
	}
	return &Mapping{data: data, mapped: true}, nil
}

func unmap(data []byte) error { return syscall.Munmap(data) }
