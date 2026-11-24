package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, 300, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"froze 300 peers (301 nodes, 50 users)", "peer host-300.example.ts.net.: user user1@example.com", "mapped peer 301 expires 2026-11-28", "compact node 301: host-300.example.ts.net."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRoundTrip(t *testing.T) {
	nm := buildNetMap(200)
	s, err := FreezeNetMap(nm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UnfreezeNetMap()
	if err != nil {
		t.Fatal(err)
	}
	// Times decode in the UTC location they were created in, so the
	// structures compare equal.
	if !reflect.DeepEqual(got, nm) {
		t.Fatal("unfrozen network map differs from the original")
	}
	if got.Peers[0].User != got.Users[got.Peers[0].User.ID] {
		t.Fatal("shared user profiles must stay shared")
	}
}
