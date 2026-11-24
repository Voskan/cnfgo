package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, 2000); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cpu limit 1, memory request 256Mi") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
	t.Log("\n" + out.String())
}

func TestQuantity(t *testing.T) {
	for _, s := range []string{"250m", "1", "0", "512Mi", "1Gi", "3Ki", "1500m", "7"} {
		var q Quantity
		if err := q.UnmarshalJSON([]byte(`"` + s + `"`)); err != nil {
			t.Fatal(err)
		}
		if got := q.String(); got != s {
			t.Errorf("%q round-trips to %q", s, got)
		}
	}
	var q Quantity
	if q.UnmarshalJSON([]byte(`"1.5x"`)) == nil {
		t.Error("accepted an invalid quantity")
	}
}

func TestPackRoundTrip(t *testing.T) {
	p := makePod(12345)
	c, err := PackPod(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := c.Unpack()
	if err != nil {
		t.Fatal(err)
	}
	if back.UID != p.UID || *back.Priority != *p.Priority || !back.StartTime.Equal(p.StartTime) ||
		back.Containers[1].Resources.MemoryLimit != p.Containers[1].Resources.MemoryLimit ||
		back.Labels["app"] != p.Labels["app"] {
		t.Fatalf("round trip mismatch: %+v", back)
	}
	if v, err := c.View().Containers().At(0).Resources().CPURequest(); err != nil || v.MilliValue() != 250 {
		t.Fatalf("CPU request %d, %v", v.MilliValue(), err)
	}
}

func BenchmarkCompactRead(b *testing.B) {
	c, err := PackPod(makePod(1))
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Getter", func(b *testing.B) {
		for b.Loop() {
			_ = c.NodeName()
		}
	})
	b.Run("View", func(b *testing.B) {
		for b.Loop() {
			_ = c.View().Containers().At(0).Image()
		}
	})
}
