package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, 50, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + out.String())
}

func TestDecisions(t *testing.T) {
	b := buildBundle(20)
	s, err := FreezeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.LenPolicy() != 23 {
		t.Fatalf("shared policies must be stored once: %d", s.LenPolicy())
	}
	want := []Decision{
		{Allow, 200, ""},
		{Deny, 403, "deny-bad-ips"},
		{Deny, 404, "admin-internal-only"},
		{Allow, 200, ""},
		{Log, 202, "app1.example.com-r3"},
		{Allow, 200, ""},
		{Allow, 200, ""},
	}
	for i, r := range testRequests() {
		if got := EvaluateView(s.Root(), r); got != want[i] || b.Evaluate(r) != want[i] {
			t.Errorf("request %d: frozen %+v, native %+v, want %+v", i, got, b.Evaluate(r), want[i])
		}
	}
}

func TestEvaluateViewAllocs(t *testing.T) {
	s, err := FreezeBundle(buildBundle(100))
	if err != nil {
		t.Fatal(err)
	}
	reqs := testRequests()
	if n := testing.AllocsPerRun(100, func() {
		for _, r := range reqs {
			EvaluateView(s.Root(), r)
		}
	}); n != 0 && !raceEnabled {
		t.Fatalf("EvaluateView allocates %v times per run", n)
	}
}

func BenchmarkEvaluate(b *testing.B) {
	bundle := buildBundle(2000)
	s, err := FreezeBundle(bundle)
	if err != nil {
		b.Fatal(err)
	}
	r := testRequests()[4]
	b.Run("Native", func(b *testing.B) {
		for b.Loop() {
			bundle.Evaluate(r)
		}
	})
	b.Run("View", func(b *testing.B) {
		v := s.Root()
		for b.Loop() {
			EvaluateView(v, r)
		}
	})
}
