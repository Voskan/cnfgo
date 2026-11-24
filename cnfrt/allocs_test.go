package cnfrt

import (
	"testing"
	"unsafe"
)

//go:noinline
func checkptrProbe(b []byte) uint64 {
	return *(*uint64)(unsafe.Pointer(unsafe.SliceData(b)))
}

// instrumented reports whether the test binary instruments pointer
// operations, as -race and -gcflags=all=-d=checkptr do. The instrumentation
// changes escape analysis, so allocation counts are not meaningful then.
var instrumented = raceEnabled || testing.AllocsPerRun(10, func() {
	var b [8]byte
	checkptrProbe(b[:])
}) != 0

// assertNoAllocs fails the test if f allocates. The check is skipped in
// instrumented builds.
func assertNoAllocs(t *testing.T, what string, f func()) {
	t.Helper()
	if instrumented {
		return
	}
	if n := testing.AllocsPerRun(200, f); n != 0 {
		t.Fatalf("%s allocates %v times per run", what, n)
	}
}
