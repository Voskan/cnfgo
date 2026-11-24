package cnfrt

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeSnapshot struct {
	strBlob string
	rows    [64]uint64
}

func newFake() *fakeSnapshot {
	return &fakeSnapshot{strBlob: strings.Repeat("node-name-", 1<<12)}
}

type fakeView struct{ s *fakeSnapshot }

func (v fakeView) Name() string { return v.s.strBlob[10:20] }

var retainedView fakeView
var retainedName string

func gcUntil(cond func() bool) bool {
	for range 20 {
		runtime.GC()
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestLeakDetectorSnapshotSensor(t *testing.T) {
	d := NewLeakDetector(0, nil)
	func() {
		s := newFake()
		h := TrackSnapshot(d, s, "fake", s.strBlob, true)
		retainedView = fakeView{s}
		h.MarkRetired()
	}()
	runtime.GC()
	var reports []LeakReport
	reports = append(reports, d.Check()...)
	if len(reports) != 1 || reports[0].Kind != LeakSnapshotRetained || reports[0].Label != "fake" {
		t.Fatalf("reports %+v", reports)
	}
	retainedView = fakeView{}
	if !gcUntil(func() bool { d.Check(); return d.Tracked() == 0 }) {
		t.Fatal("entry not dropped after the snapshot and blob were collected")
	}
}

func TestLeakDetectorStringBlobSensor(t *testing.T) {
	var got []LeakReport
	d := NewLeakDetector(0, func(r LeakReport) { got = append(got, r) })
	func() {
		s := newFake()
		h := TrackSnapshot(d, s, "fake", s.strBlob, true)
		retainedName = fakeView{s}.Name()
		h.MarkRetired()
	}()
	if !gcUntil(func() bool { d.Check(); return len(got) > 0 }) {
		t.Fatal("blob sensor did not fire")
	}
	if got[0].Kind != LeakStringBlobRetained || got[0].BlobBytes != 10<<12 {
		t.Fatalf("report %+v", got[0])
	}
	if retainedName != "node-name-" {
		t.Fatal("unexpected substring")
	}
	retainedName = ""
	if !gcUntil(func() bool { d.Check(); return d.Tracked() == 0 }) {
		t.Fatal("entry not dropped")
	}
	for _, r := range got {
		if r.Kind == LeakSnapshotRetained {
			t.Fatal("snapshot sensor fired although the snapshot was collected")
		}
	}
}

func TestLeakDetectorNoFalsePositive(t *testing.T) {
	d := NewLeakDetector(0, nil)
	func() {
		s := newFake()
		TrackSnapshot(d, s, "fake", s.strBlob, true).MarkRetired()
	}()
	if !gcUntil(func() bool { return len(d.Check()) == 0 && d.Tracked() == 0 }) {
		t.Fatal("clean snapshot reported or not dropped")
	}
	if TrackSnapshot[fakeSnapshot](nil, nil, "", "", true) != nil {
		t.Fatal("nil detector")
	}
	var h *LeakHandle
	h.MarkRetired()
}

func TestLeakDetectorSkipsSmallAndBorrowedBlobs(t *testing.T) {
	d := NewLeakDetector(0, nil)
	s := &fakeSnapshot{strBlob: strings.Repeat("x", 15)}
	h := TrackSnapshot(d, s, "small", s.strBlob, true)
	if h.e.hasBlob {
		t.Fatal("blob sensor armed for a blob below 16 bytes")
	}
	m := newFake()
	if TrackSnapshot(d, m, "mmap", m.strBlob, false).e.hasBlob {
		t.Fatal("blob sensor armed for a blob the snapshot does not own")
	}
	runtime.KeepAlive(s)
	runtime.KeepAlive(m)
}

func TestLeakDetectorBackgroundTimer(t *testing.T) {
	ch := make(chan LeakReport, 4)
	d := EnableLeakDetector(5*time.Millisecond, func(r LeakReport) { ch <- r })
	defer DisableLeakDetector()
	if GlobalLeakDetector() != d {
		t.Fatal("global detector")
	}
	s := newFake()
	TrackSnapshot(d, s, "timer", s.strBlob, true).MarkRetired()
	select {
	case r := <-ch:
		if r.Kind != LeakSnapshotRetained || r.Age < 5*time.Millisecond {
			t.Fatalf("report %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timer did not fire")
	}
	runtime.KeepAlive(s)
}

// TestLeakDetectorDropsUnretiredSnapshots checks that snapshots that are
// replaced without MarkRetired do not accumulate in the detector.
func TestLeakDetectorDropsUnretiredSnapshots(t *testing.T) {
	d := NewLeakDetector(time.Hour, nil)
	keep := newFake()
	TrackSnapshot(d, keep, "kept", keep.strBlob, true)
	func() {
		for range 50 {
			s := newFake()
			TrackSnapshot(d, s, "dropped", s.strBlob, true)
			b := &fakeSnapshot{}
			TrackSnapshot(d, b, "borrowed", "borrowed blob of 24 bytes", false)
		}
	}()
	if !gcUntil(func() bool { return d.Tracked() == 1 }) {
		t.Fatalf("%d entries tracked after 100 snapshots were collected, want 1", d.Tracked())
	}
	runtime.KeepAlive(keep)
}

// TestLeakDetectorReportsBlobWithoutRetire checks that a substring pinning
// the blob of a collected snapshot is reported by the background timer even
// if MarkRetired was never called.
func TestLeakDetectorReportsBlobWithoutRetire(t *testing.T) {
	ch := make(chan LeakReport, 4)
	d := NewLeakDetector(5*time.Millisecond, func(r LeakReport) { ch <- r })
	func() {
		s := newFake()
		TrackSnapshot(d, s, "unretired", s.strBlob, true)
		retainedName = fakeView{s}.Name()
	}()
	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		select {
		case r := <-ch:
			if r.Kind != LeakStringBlobRetained || r.Label != "unretired" {
				t.Fatalf("report %+v", r)
			}
			retainedName = ""
			if !gcUntil(func() bool { return d.Tracked() == 0 }) {
				t.Fatal("entry not dropped after the blob was released")
			}
			return
		case <-deadline:
			t.Fatal("blob retention was not reported")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
