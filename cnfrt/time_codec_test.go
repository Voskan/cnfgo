package cnfrt

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func roundTripTime(t *testing.T, lt *LocTable, in time.Time) time.Time {
	t.Helper()
	ft := lt.Encode(in)
	if !ft.Valid(len(lt.Entries)) {
		t.Fatalf("encoded %v is not valid: %+v", in, ft)
	}
	return DecodeTime(ft, lt.Locations())
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	return loc
}

func sameTime(t *testing.T, in, out time.Time) {
	t.Helper()
	if !in.Equal(out) {
		t.Fatalf("instant changed: %v -> %v", in, out)
	}
	if in.Nanosecond() != out.Nanosecond() || in.Unix() != out.Unix() {
		t.Fatalf("unix/nsec changed: %v -> %v", in, out)
	}
	n1, o1 := in.Zone()
	n2, o2 := out.Zone()
	if n1 != n2 || o1 != o2 {
		t.Fatalf("zone changed: %s%+d -> %s%+d", n1, o1, n2, o2)
	}
	if in.Location().String() != out.Location().String() {
		t.Fatalf("location changed: %q -> %q", in.Location(), out.Location())
	}
	if in.IsZero() != out.IsZero() {
		t.Fatalf("IsZero changed for %v", in)
	}
}

func TestTimeUTC(t *testing.T) {
	var lt LocTable
	for _, in := range []time.Time{
		{},
		time.Date(2026, 11, 22, 20, 0, 0, 123456789, time.UTC),
		time.Unix(0, 0).UTC(),
		time.Date(-50000, 3, 1, 0, 0, 0, 1, time.UTC),
		time.Date(300000000, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Unix(math.MaxInt64, 999999999).UTC(),
		time.Unix(math.MinInt64, 0).UTC(),
	} {
		out := roundTripTime(t, &lt, in)
		if out != in {
			t.Fatalf("UTC round trip not identical: %#v -> %#v", in, out)
		}
	}
	if len(lt.Entries) != 0 {
		t.Fatal("UTC times populated the location table")
	}
}

func TestTimeZeroInstantKeepsLocation(t *testing.T) {
	msk := mustLoad(t, "Europe/Moscow")
	var lt LocTable
	zero := time.Time{}.In(msk)
	if !zero.IsZero() {
		t.Fatal("precondition")
	}
	out := roundTripTime(t, &lt, zero)
	sameTime(t, zero, out)
	if out.Location() == time.UTC {
		t.Fatal("zero instant lost its location")
	}
	d := time.Date(1, 1, 1, 3, 0, 0, 0, msk)
	sameTime(t, d, roundTripTime(t, &lt, d))
}

func TestTimeRFC3339Offsets(t *testing.T) {
	var lt LocTable
	for _, s := range []string{"2026-11-22T20:00:00+05:30", "2026-11-22T20:00:00-07:00", "1999-01-01T00:00:00.5+14:00"} {
		in, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		out := roundTripTime(t, &lt, in)
		sameTime(t, in, out)
		if out.Format(time.RFC3339Nano) != in.Format(time.RFC3339Nano) {
			t.Fatalf("format changed: %s -> %s", in.Format(time.RFC3339Nano), out.Format(time.RFC3339Nano))
		}
	}
}

func TestTimeFixedZoneVersusIANA(t *testing.T) {
	cet := mustLoad(t, "CET")
	fixed := time.FixedZone("CET", 3600)
	summer := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var lt LocTable
	outFixed := roundTripTime(t, &lt, summer.In(fixed))
	outIANA := roundTripTime(t, &lt, summer.In(cet))
	if _, off := outFixed.Zone(); off != 3600 {
		t.Fatalf("FixedZone(CET) became DST-aware: offset %d", off)
	}
	if _, off := outIANA.Zone(); off != 7200 {
		t.Fatalf("IANA CET lost DST: offset %d", off)
	}
	later := outFixed.Add(-180 * 24 * time.Hour)
	if _, off := later.Zone(); off != 3600 {
		t.Fatalf("decoded fixed zone is not fixed")
	}
	if n := len(lt.Entries); n != 2 {
		t.Fatalf("expected 2 location entries, got %d", n)
	}
}

func TestTimeDST(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	var lt LocTable
	for _, in := range []time.Time{
		time.Date(2026, 3, 8, 1, 59, 59, 0, ny),
		time.Date(2026, 3, 8, 3, 0, 0, 0, ny),
		time.Date(2026, 11, 1, 1, 30, 0, 0, ny),
		time.Date(2026, 11, 1, 1, 30, 0, 0, ny).Add(time.Hour),
		time.Date(2026, 11, 1, 2, 30, 0, 0, ny),
	} {
		out := roundTripTime(t, &lt, in)
		sameTime(t, in, out)
		r, _ := ResolveLocation("America/New_York")
		if out.Location() != r {
			t.Fatal("location pointer is not the cached instance")
		}
	}
}

func TestTimeLocal(t *testing.T) {
	in := time.Date(2026, 6, 1, 12, 0, 0, 0, time.Local)
	var lt LocTable
	out := roundTripTime(t, &lt, in)
	if !out.Equal(in) {
		t.Fatal("instant")
	}
	n1, o1 := in.Zone()
	n2, o2 := out.Zone()
	if n1 != n2 || o1 != o2 {
		t.Fatalf("Local default encoding changed zone %s%d -> %s%d", n1, o1, n2, o2)
	}
	lt2 := LocTable{PreserveLocal: true}
	out2 := roundTripTime(t, &lt2, in)
	if out2.Location() != time.Local {
		t.Fatal("PreserveLocal did not restore time.Local")
	}
}

func TestTimeLargeOffsetsAndManyZones(t *testing.T) {
	var lt LocTable
	big := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("BIG", 1_000_000))
	sameTime(t, big, roundTripTime(t, &lt, big))
	neg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", -600_000))
	out := roundTripTime(t, &lt, neg)
	sameTime(t, neg, out)
	var fts []FlatTime
	var ins []time.Time
	for i := range 5000 {
		in := time.Date(2026, 1, 1, 0, 0, i, 0, time.FixedZone(fmt.Sprintf("Z%04d", i), i))
		ins = append(ins, in)
		fts = append(fts, lt.Encode(in))
	}
	locs := lt.Locations()
	indexed := 0
	for i, ft := range fts {
		if ft.Kind() == LocIndexed {
			indexed++
		}
		if !ft.Valid(len(locs)) {
			t.Fatalf("invalid flat time %d", i)
		}
		sameTime(t, ins[i], DecodeTime(ft, locs))
	}
	if indexed == 0 {
		t.Fatal("expected LocIndexed fallback beyond 4095 fixed zones")
	}
}

func TestFlatTimeValidity(t *testing.T) {
	if (FlatTime{Meta: 1e9}).Valid(0) {
		t.Fatal("nsec overflow accepted")
	}
	if (FlatTime{Meta: LocIANA << 30}).Valid(5) {
		t.Fatal("IANA without index accepted")
	}
	if (FlatTime{Meta: LocIANA<<30 | 6<<32}).Valid(5) {
		t.Fatal("index out of range accepted")
	}
	if (FlatTime{Meta: 1 << 40}).Valid(5) {
		t.Fatal("UTC with payload accepted")
	}
}

func TestResolveLocationCache(t *testing.T) {
	a, err := ResolveLocation("Asia/Tokyo")
	if err != nil {
		t.Skip(err)
	}
	b, _ := ResolveLocation("Asia/Tokyo")
	if a != b {
		t.Fatal("cache returned different pointers")
	}
	if l, _ := ResolveLocation(""); l != time.UTC {
		t.Fatal("empty name")
	}
	if _, err := ResolveLocation("No/Such_Zone"); err == nil {
		t.Fatal("bogus zone resolved")
	}
	fixedLocs.m.Store(nil) // other tests may have filled the bounded cache
	if FixedLocation("X", 60) != FixedLocation("X", 60) {
		t.Fatal("fixed cache")
	}
	for i := range 2 * maxLocCache {
		FixedLocation("fill", i)
	}
	if n := len(*fixedLocs.m.Load()); n != maxLocCache {
		t.Fatalf("fixed cache grew to %d entries", n)
	}
	if l := FixedLocation("fill", 10*maxLocCache); l.String() != "fill" {
		t.Fatal("uncached fixed location")
	}
}

func TestDecodeTimeAllocs(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	fixedLocs.m.Store(nil) // other tests may have filled the bounded cache
	var lt LocTable
	fts := []FlatTime{
		lt.Encode(time.Now().UTC()),
		lt.Encode(time.Now().In(ny)),
		lt.Encode(time.Now().In(time.FixedZone("", 19800))),
		lt.Encode(time.Now().In(time.FixedZone("IST", 19800))),
	}
	locs := lt.Locations()
	for _, ft := range fts {
		DecodeTime(ft, locs)
		assertNoAllocs(t, "DecodeTime", func() { _ = DecodeTime(ft, locs) })
	}
}
