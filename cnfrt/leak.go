package cnfrt

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"weak"
)

// LeakKind classifies a LeakReport.
type LeakKind uint8

const (
	// LeakSnapshotRetained reports that a snapshot is still reachable
	// (typically through retained View values) longer than the threshold
	// after MarkRetired.
	LeakSnapshotRetained LeakKind = iota + 1
	// LeakStringBlobRetained reports that a snapshot has been collected but
	// its string blob is still reachable longer than the threshold, which
	// means a substring returned by a string getter is pinning the whole
	// blob.
	LeakStringBlobRetained
)

// String returns the name of the leak kind.
func (k LeakKind) String() string {
	switch k {
	case LeakSnapshotRetained:
		return "snapshot retained after MarkRetired"
	case LeakStringBlobRetained:
		return "string blob retained after snapshot was collected"
	}
	return "unknown leak"
}

// LeakReport describes a detected retention.
type LeakReport struct {
	Kind LeakKind
	// Label identifies the snapshot type.
	Label string
	// Since is the time at which the retention window started: the
	// MarkRetired call for LeakSnapshotRetained, or the observed collection
	// of the snapshot for LeakStringBlobRetained.
	Since time.Time
	// Age is the time elapsed since Since when the leak was detected.
	Age time.Duration
	// BlobBytes is the size of the retained string blob.
	BlobBytes int
}

type leakEntry struct {
	d         *LeakDetector
	label     string
	snapAlive func() bool
	blob      weak.Pointer[byte]
	hasBlob   bool
	blobBytes int

	retiredAt   atomic.Int64
	snapGoneAt  atomic.Int64
	blobGone    atomic.Bool
	reportedS   bool
	reportedB   bool
	scheduledAt atomic.Int64
}

// LeakDetector watches snapshots for two kinds of retention using two
// independent sensors: a weak pointer and cleanup attached to the snapshot
// itself, and a weak pointer and cleanup attached to the backing array of
// its string blob.
type LeakDetector struct {
	threshold time.Duration
	onLeak    func(LeakReport)

	mu      sync.Mutex
	entries map[*leakEntry]struct{}
}

// LeakHandle is the per-snapshot registration returned by TrackSnapshot.
// It holds no strong reference to the snapshot or its string blob.
type LeakHandle struct {
	d *LeakDetector
	e *leakEntry
}

var globalLeak atomic.Pointer[LeakDetector]

// EnableLeakDetector installs a process-wide leak detector. Snapshots
// created afterwards by generated code are tracked automatically. onLeak is
// called from a background goroutine (or from Check) once per detected
// leak.
func EnableLeakDetector(threshold time.Duration, onLeak func(LeakReport)) *LeakDetector {
	d := NewLeakDetector(threshold, onLeak)
	globalLeak.Store(d)
	return d
}

// DisableLeakDetector removes the process-wide leak detector.
func DisableLeakDetector() { globalLeak.Store(nil) }

// GlobalLeakDetector returns the process-wide leak detector, or nil.
func GlobalLeakDetector() *LeakDetector { return globalLeak.Load() }

// NewLeakDetector returns a detector that is not installed globally.
func NewLeakDetector(threshold time.Duration, onLeak func(LeakReport)) *LeakDetector {
	return &LeakDetector{threshold: threshold, onLeak: onLeak, entries: make(map[*leakEntry]struct{})}
}

// TrackSnapshot registers snap with d. ownsBlob reports whether strBlob
// lies in a heap allocation that belongs to the snapshot (a frozen snapshot,
// or one loaded from a private copy of its image); the string-blob sensor is
// armed only for such blobs of at least 16 bytes, because a blob that aliases
// caller memory (a memory-mapped file, a static array or a buffer the caller
// keeps) is not freed with the snapshot, and smaller pointer-free
// allocations may share a block with unrelated objects. It returns nil if d
// is nil.
//
// Entries are dropped automatically: once the snapshot has been collected
// and its blob (if tracked) is gone too, whether or not MarkRetired was
// called. If the snapshot is collected while its blob is still reachable, a
// LeakStringBlobRetained report is emitted after the threshold.
func TrackSnapshot[S any](d *LeakDetector, snap *S, label string, strBlob string, ownsBlob bool) *LeakHandle {
	if d == nil || snap == nil {
		return nil
	}
	wp := weak.Make(snap)
	e := &leakEntry{d: d, label: label, snapAlive: func() bool { return wp.Value() != nil }}
	if ownsBlob && len(strBlob) >= 16 {
		p := unsafe.StringData(strBlob)
		e.blob = weak.Make(p)
		e.hasBlob = true
		e.blobBytes = len(strBlob)
	}
	d.mu.Lock()
	d.entries[e] = struct{}{}
	d.mu.Unlock()
	runtime.AddCleanup(snap, (*leakEntry).snapCollected, e)
	if e.hasBlob {
		runtime.AddCleanup(unsafe.StringData(strBlob), (*leakEntry).blobCollected, e)
	}
	return &LeakHandle{d: d, e: e}
}

// snapCollected runs when the snapshot has been collected. If its blob is
// still reachable the retention window starts and a check is scheduled;
// otherwise the entry is no longer needed.
func (e *leakEntry) snapCollected() {
	e.snapGoneAt.CompareAndSwap(0, time.Now().UnixNano())
	if e.hasBlob && !e.blobGone.Load() {
		e.d.schedule(e)
		return
	}
	e.d.forget(e)
}

// blobCollected runs when the string blob has been collected.
func (e *leakEntry) blobCollected() {
	e.blobGone.Store(true)
	if e.snapGoneAt.Load() != 0 {
		e.d.forget(e)
	}
}

func (d *LeakDetector) forget(e *leakEntry) {
	d.mu.Lock()
	delete(d.entries, e)
	d.mu.Unlock()
}

// MarkRetired records that the snapshot is no longer expected to be used.
// If the snapshot is still reachable after the detector's threshold, a
// LeakSnapshotRetained report is emitted.
func (h *LeakHandle) MarkRetired() {
	if h == nil {
		return
	}
	now := time.Now()
	if !h.e.retiredAt.CompareAndSwap(0, now.UnixNano()) {
		return
	}
	h.d.schedule(h.e)
}

func (d *LeakDetector) schedule(e *leakEntry) {
	if d.threshold <= 0 || !e.scheduledAt.CompareAndSwap(0, 1) {
		return
	}
	var tick func()
	tick = func() {
		d.Check()
		d.mu.Lock()
		_, live := d.entries[e]
		// Stop polling once every report that can currently fire for e has
		// already been emitted. If the snapshot is still alive and reported,
		// reset scheduledAt so snapCollected can re-arm the timer if the
		// snapshot is eventually collected while its blob remains pinned.
		done := !live || (e.snapGoneAt.Load() != 0 && (!e.hasBlob || e.reportedB)) || (e.snapGoneAt.Load() == 0 && e.reportedS)
		if done {
			e.scheduledAt.Store(0)
		}
		d.mu.Unlock()
		if !done {
			time.AfterFunc(d.threshold, tick)
		}
	}
	time.AfterFunc(d.threshold, tick)
}

// Check evaluates every tracked snapshot, calls onLeak for newly detected
// leaks and returns them. Entries whose snapshot and blob have both been
// collected are dropped.
func (d *LeakDetector) Check() []LeakReport {
	now := time.Now()
	var out []LeakReport
	d.mu.Lock()
	for e := range d.entries {
		snapAlive := e.snapAlive()
		if !snapAlive {
			e.snapGoneAt.CompareAndSwap(0, now.UnixNano())
		}
		blobAlive := e.hasBlob && e.blob.Value() != nil && !e.blobGone.Load()
		if r := e.retiredAt.Load(); snapAlive && r != 0 && !e.reportedS {
			since := time.Unix(0, r)
			if age := now.Sub(since); age >= d.threshold {
				e.reportedS = true
				out = append(out, LeakReport{Kind: LeakSnapshotRetained, Label: e.label, Since: since, Age: age, BlobBytes: e.blobBytes})
			}
		}
		if g := e.snapGoneAt.Load(); !snapAlive && blobAlive && g != 0 && !e.reportedB {
			since := time.Unix(0, g)
			if age := now.Sub(since); age >= d.threshold {
				e.reportedB = true
				out = append(out, LeakReport{Kind: LeakStringBlobRetained, Label: e.label, Since: since, Age: age, BlobBytes: e.blobBytes})
			} else {
				d.schedule(e)
			}
		} else if !snapAlive && blobAlive && !e.reportedB {
			d.schedule(e)
		}
		if !snapAlive && !blobAlive {
			delete(d.entries, e)
		}
	}
	d.mu.Unlock()
	if d.onLeak != nil {
		for _, r := range out {
			d.onLeak(r)
		}
	}
	return out
}

// Tracked returns the number of snapshots currently tracked.
func (d *LeakDetector) Tracked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}
