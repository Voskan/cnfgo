package cnfrt

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FlatTime is the 16-byte pointer-free encoding of a time.Time.
//
// UnixSec is the exact result of t.Unix(). Meta packs the nanosecond (bits
// 0..29), the location kind (bits 30..31) and kind-specific location data
// (bits 32..63):
//
//   - LocUTC: no data.
//   - LocFixed: bits 32..51 hold the signed UTC offset in seconds and bits
//     52..63 hold the 1-based index of the (name, offset) pair in the
//     snapshot's location table, or 0 for an unnamed fixed zone such as the
//     one produced by parsing "+05:30" with time.RFC3339.
//   - LocIANA: bits 32..63 hold the 1-based index of a named tz database
//     location in the location table.
//   - LocIndexed: bits 32..63 hold the 1-based index of a fixed zone whose
//     index or offset does not fit the LocFixed layout.
//
// There is deliberately no special case for t.IsZero(), so the location of
// a zero instant such as time.Date(1, 1, 1, 3, 0, 0, 0, msk) survives a
// round trip. Monotonic clock readings are not preserved.
type FlatTime struct {
	UnixSec int64
	Meta    uint64
}

// Location kinds stored in FlatTime.Meta.
const (
	LocUTC     = 0
	LocFixed   = 1
	LocIANA    = 2
	LocIndexed = 3
)

const (
	nsecMask       = 1<<30 - 1
	fixedOffBits   = 20
	fixedOffMax    = 1<<(fixedOffBits-1) - 1
	fixedOffMin    = -(1 << (fixedOffBits - 1))
	fixedNameMax   = 1<<12 - 1
	locTableMaxLen = 1<<32 - 2
)

// Kind returns the location kind of t.
func (t FlatTime) Kind() uint8 { return uint8(t.Meta>>30) & 3 }

// Nanosecond returns the nanosecond component of t.
func (t FlatTime) Nanosecond() int64 { return int64(t.Meta & nsecMask) }

// LocIndex returns the 1-based location table index referenced by t, or 0
// if t does not reference the table.
func (t FlatTime) LocIndex() uint32 {
	switch t.Kind() {
	case LocFixed:
		return uint32(t.Meta >> 52)
	case LocIANA, LocIndexed:
		return uint32(t.Meta >> 32)
	}
	return 0
}

// FixedOffset returns the offset stored inline in a LocFixed time.
func (t FlatTime) FixedOffset() int {
	v := int64(t.Meta<<(64-52)) >> (64 - fixedOffBits)
	return int(v)
}

// Valid reports whether t is structurally valid for a location table of
// length nlocs.
func (t FlatTime) Valid(nlocs int) bool {
	idx := t.LocIndex()
	switch t.Kind() {
	case LocUTC:
		if t.Meta>>32 != 0 {
			return false
		}
	case LocIANA, LocIndexed:
		if idx == 0 {
			return false
		}
	}
	return uint64(idx) <= uint64(nlocs) && t.Meta&nsecMask < 1e9
}

// LocEntry is one entry of a snapshot's location table.
type LocEntry struct {
	// Kind is LocFixed for fixed zones (including those referenced through
	// LocIndexed) and LocIANA for tz database locations.
	Kind uint8
	// Name is the zone name for fixed zones, or the location name passed to
	// ResolveLocation for tz database locations.
	Name string
	// Offset is the UTC offset of a fixed zone in seconds.
	Offset int32
	// Loc is the resolved location.
	Loc *time.Location
}

// Resolve resolves e.Loc from its name and offset.
func (e *LocEntry) Resolve() error {
	if e.Kind == LocIANA {
		loc, err := ResolveLocation(e.Name)
		if err != nil {
			return err
		}
		e.Loc = loc
		return nil
	}
	e.Loc = FixedLocation(e.Name, int(e.Offset))
	return nil
}

type fixedKey struct {
	name   string
	offset int
}

type locClass struct {
	fixed    bool
	idx      uint32
	metaBits uint64 // precomputed Meta bits above nsecMask when fixed is true
}

// LocTable assigns location table indices while freezing.
type LocTable struct {
	Entries []LocEntry
	byLoc   map[*time.Location]locClass
	byFixed map[fixedKey]uint32
	// PreserveLocal encodes time.Local as the tz database location "Local"
	// instead of the fixed zone in effect at each instant.
	PreserveLocal bool
}

// Encode converts t to its flat form, registering its location in the
// table if needed.
func (lt *LocTable) Encode(t time.Time) FlatTime {
	ft := FlatTime{UnixSec: t.Unix(), Meta: uint64(t.Nanosecond())}
	loc := t.Location()
	if loc == time.UTC {
		return ft
	}
	if loc == time.Local {
		if lt.PreserveLocal {
			return lt.iana(ft, t, loc, "Local")
		}
		name, off := t.Zone()
		return lt.fixed(ft, name, off)
	}
	if c, ok := lt.byLoc[loc]; ok {
		if c.fixed {
			ft.Meta |= c.metaBits
			return ft
		}
		ft.Meta |= LocIANA<<30 | uint64(c.idx)<<32
		return ft
	}
	name := loc.String()
	if name == "" || isFixedZone(loc) {
		_, off := t.Zone()
		ft = lt.fixed(ft, name, off)
		if lt.byLoc == nil {
			lt.byLoc = make(map[*time.Location]locClass)
		}
		lt.byLoc[loc] = locClass{fixed: true, metaBits: ft.Meta &^ nsecMask}
		return ft
	}
	return lt.iana(ft, t, loc, name)
}

func (lt *LocTable) add(e LocEntry) uint32 {
	if uint64(len(lt.Entries)) >= locTableMaxLen {
		Fail(ErrCapacityExceeded)
	}
	lt.Entries = append(lt.Entries, e)
	return uint32(len(lt.Entries))
}

func (lt *LocTable) iana(ft FlatTime, t time.Time, loc *time.Location, name string) FlatTime {
	c, ok := lt.byLoc[loc]
	if !ok {
		resolved, err := ResolveLocation(name)
		if err != nil {
			// The location is not in the system tz database (for example,
			// one built with time.LoadLocationFromTZData). Fall back to
			// encoding the fixed offset active at this instant so that
			// .cnf images and Compact values remain self-contained.
			zname, zoff := t.Zone()
			return lt.fixed(ft, zname, zoff)
		}
		if lt.byLoc == nil {
			lt.byLoc = make(map[*time.Location]locClass)
		}
		c = locClass{idx: lt.add(LocEntry{Kind: LocIANA, Name: name, Loc: resolved})}
		lt.byLoc[loc] = c
	}
	ft.Meta |= LocIANA<<30 | uint64(c.idx)<<32
	return ft
}

func (lt *LocTable) fixed(ft FlatTime, name string, off int) FlatTime {
	if int(int32(off)) != off {
		Fail(ErrCapacityExceeded)
	}
	var idx uint32
	if name != "" || off < fixedOffMin || off > fixedOffMax {
		k := fixedKey{name, off}
		var ok bool
		idx, ok = lt.byFixed[k]
		if !ok {
			if lt.byFixed == nil {
				lt.byFixed = make(map[fixedKey]uint32)
			}
			idx = lt.add(LocEntry{Kind: LocFixed, Name: name, Offset: int32(off), Loc: FixedLocation(name, off)})
			lt.byFixed[k] = idx
		}
	}
	if idx > fixedNameMax || off < fixedOffMin || off > fixedOffMax {
		ft.Meta |= LocIndexed<<30 | uint64(idx)<<32
		return ft
	}
	ft.Meta |= LocFixed<<30 | (uint64(int64(off))&(1<<fixedOffBits-1))<<32 | uint64(idx)<<52
	return ft
}

// Locations returns the resolved locations of the table, indexed by
// location index minus one.
func (lt *LocTable) Locations() []*time.Location {
	if len(lt.Entries) == 0 {
		return nil
	}
	locs := make([]*time.Location, len(lt.Entries))
	for i := range lt.Entries {
		locs[i] = lt.Entries[i].Loc
	}
	return locs
}

// DecodeTime converts ft back to a time.Time using the resolved location
// table locs. It does not allocate.
func DecodeTime(ft FlatTime, locs []*time.Location) time.Time {
	t := time.Unix(ft.UnixSec, int64(ft.Meta&nsecMask))
	switch ft.Kind() {
	case LocUTC:
		return t.UTC()
	case LocFixed:
		if idx := ft.Meta >> 52; idx != 0 {
			return t.In(locs[idx-1])
		}
		return t.In(FixedLocation("", ft.FixedOffset()))
	default:
		return t.In(locs[(ft.Meta>>32)-1])
	}
}

// isFixedZone reports whether loc was created by time.FixedZone. It
// inspects the location's internal transition table: a fixed zone has a
// single zone, a single transition at the beginning of time and no
// extension rule. If the internal layout cannot be inspected, it falls back
// to treating names unknown to the tz database as fixed zones.
func isFixedZone(loc *time.Location) bool {
	if fixed, ok := inspectFixedZone(loc); ok {
		return fixed
	}
	_, err := time.LoadLocation(loc.String())
	return err != nil
}

func inspectFixedZone(loc *time.Location) (fixed, ok bool) {
	defer func() {
		if recover() != nil {
			fixed, ok = false, false
		}
	}()
	v := reflect.ValueOf(loc).Elem()
	zone, tx, ext := v.FieldByName("zone"), v.FieldByName("tx"), v.FieldByName("extend")
	if zone.Kind() != reflect.Slice || tx.Kind() != reflect.Slice || ext.Kind() != reflect.String {
		return false, false
	}
	if zone.Len() != 1 || tx.Len() != 1 || ext.Len() != 0 {
		return false, true
	}
	when := tx.Index(0).FieldByName("when")
	if when.Kind() != reflect.Int64 {
		return false, false
	}
	return when.Int() == -1<<63, true
}

// maxLocCache bounds the process-wide location caches.
const maxLocCache = 512

type ianaCache struct {
	mu sync.Mutex
	m  atomic.Pointer[map[string]*time.Location]
}

var ianaLocs ianaCache

// ResolveLocation returns the tz database location with the given name.
// Results are kept in a bounded, lock-free copy-on-write cache, so repeated
// calls with the same name return the same *time.Location within the
// process (for up to 512 distinct names).
func ResolveLocation(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	if name == "Local" {
		return time.Local, nil
	}
	if m := ianaLocs.m.Load(); m != nil {
		if loc, ok := (*m)[name]; ok {
			return loc, nil
		}
	}
	name = strings.Clone(name)
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	ianaLocs.mu.Lock()
	defer ianaLocs.mu.Unlock()
	old := ianaLocs.m.Load()
	if old != nil {
		if cached, ok := (*old)[name]; ok {
			return cached, nil
		}
		if len(*old) >= maxLocCache {
			return loc, nil
		}
	}
	next := make(map[string]*time.Location, 1+lenMap(old))
	if old != nil {
		for k, v := range *old {
			next[k] = v
		}
	}
	next[name] = loc
	ianaLocs.m.Store(&next)
	return loc, nil
}

func lenMap[K comparable, V any](m *map[K]V) int {
	if m == nil {
		return 0
	}
	return len(*m)
}

type fixedCache struct {
	mu sync.Mutex
	m  atomic.Pointer[map[fixedKey]*time.Location]
}

var fixedLocs fixedCache

// maxFixedNameCache is the longest zone name kept in the FixedLocation
// cache; longer names are rare and would let crafted input pin memory.
const maxFixedNameCache = 64

// FixedLocation returns time.FixedZone(name, offset) from a bounded,
// lock-free copy-on-write cache, so decoding fixed-zone times does not
// allocate. Names longer than 64 bytes are not cached.
func FixedLocation(name string, offset int) *time.Location {
	if len(name) > maxFixedNameCache {
		return time.FixedZone(strings.Clone(name), offset)
	}
	k := fixedKey{name, offset}
	if m := fixedLocs.m.Load(); m != nil {
		if loc, ok := (*m)[k]; ok {
			return loc
		}
	}
	name = strings.Clone(name)
	k.name = name
	fixedLocs.mu.Lock()
	defer fixedLocs.mu.Unlock()
	old := fixedLocs.m.Load()
	if old != nil {
		if loc, ok := (*old)[k]; ok {
			return loc
		}
	}
	loc := time.FixedZone(name, offset)
	if old != nil && len(*old) >= maxLocCache {
		return loc
	}
	next := make(map[fixedKey]*time.Location, 1+lenMap(old))
	if old != nil {
		for kk, v := range *old {
			next[kk] = v
		}
	}
	next[k] = loc
	fixedLocs.m.Store(&next)
	return loc
}
