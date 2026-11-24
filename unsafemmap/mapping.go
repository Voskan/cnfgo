package unsafemmap

import (
	"errors"
	"runtime"
	"sync/atomic"

	"github.com/Voskan/cnfgo/cnfrt"
)

// ErrEmptyFile is returned by Open for empty files.
var ErrEmptyFile = errors.New("unsafemmap: empty file")

// Mapping is a read-only view of a file's contents. It holds no reference
// to the snapshots built on top of it, so it can serve as the argument of a
// cleanup attached to them.
type Mapping struct {
	data   []byte
	mapped bool // data is a memory mapping, not a heap copy
	closed atomic.Bool
}

// Bytes returns the contents of the mapping, or nil after Close. The slice
// must not be modified (writing to it faults) and must not be used after
// Close.
func (m *Mapping) Bytes() []byte {
	if m.closed.Load() {
		return nil
	}
	return m.data
}

// Len returns the size of the mapping in bytes.
func (m *Mapping) Len() int { return len(m.data) }

// IsMapped reports whether the contents are memory-mapped rather than read
// into the heap (as on platforms without mmap support).
func (m *Mapping) IsMapped() bool { return m.mapped }

// Close releases the mapping. It is idempotent and safe for concurrent use:
// only the first call unmaps, later calls return nil.
func (m *Mapping) Close() error {
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	if m.mapped && len(m.data) > 0 {
		return unmap(m.data)
	}
	return nil
}

// Closed reports whether Close has been called.
func (m *Mapping) Closed() bool { return m.closed.Load() }

// AttachCleanup arranges for m to be closed once owner becomes unreachable.
// The cleanup holds only m, never owner, so it does not keep owner alive.
func AttachCleanup[S any](owner *S, m *Mapping) {
	runtime.AddCleanup(owner, func(m *Mapping) { m.Close() }, m)
}

// FaultSafe calls fn and converts a memory fault raised while it runs (for
// example a SIGBUS from reading a mapped file that another process
// truncated) into an error wrapping cnfrt.ErrMmapFault. Panics that are not
// memory faults are propagated.
func FaultSafe(fn func() error) error { return cnfrt.FaultSafe(fn) }
