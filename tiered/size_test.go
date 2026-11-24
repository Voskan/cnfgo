package tiered

import (
	"testing"
	"unsafe"
)

func TestShardSize(t *testing.T) {
	if s := unsafe.Sizeof(deltaShard[string, int]{}); unsafe.Sizeof(uintptr(0)) == 8 && s != 128 {
		t.Fatalf("deltaShard is %d bytes, want 128", s)
	}
}
