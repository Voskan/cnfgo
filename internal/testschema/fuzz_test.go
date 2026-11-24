package testschema

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

// resignCompact recomputes the trailing CRC32C of a slim compact value.
func resignCompact(raw []byte) {
	if len(raw) < 4 {
		return
	}
	binary.LittleEndian.PutUint32(raw[len(raw)-4:], cnfrt.CRC32C(raw[:len(raw)-4]))
}

// FuzzParseCompact feeds mutated compact values to ParseCompactEndpoint
// with VerifyUntrusted. Mutations flip one byte, optionally re-signing the
// embedded image so that checksums no longer reject it. Every getter and a
// full traversal of an accepted value must complete without panicking;
// codec payloads are opaque to validation, so decoding them may still
// report an error.
func FuzzParseCompact(f *testing.F) {
	full := &Endpoint{
		Name: "api", Port: 443, Weight: -0.25, Up: true,
		Addr: netip.MustParseAddr("fe80::2%en0"), Prefix: netip.MustParsePrefix("2001:db8::/32"),
		AP: netip.MustParseAddrPort("[fe80::3%lo0]:8080"), Seen: time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("CET", 3600)),
		Opt: ptr(int32(7)), OptName: ptr("n"), Arr: [2]uint16{7, 65535}, Blob: []byte("xyz"), Tags: []string{"a", "b"},
		Meta: Meta{Key: "m", Value: "v", Count: 3}, Fee: NewMoney(3, "GBP"),
	}
	for _, ep := range []*Endpoint{full, {}, {Name: "x", Tags: []string{}}} {
		c, err := PackEndpoint(ep)
		if err != nil {
			f.Fatal(err)
		}
		raw := []byte(c.Raw())
		f.Add(raw, uint16(0), byte(0), false)
		f.Add(raw, uint16(len(raw)/2), byte(1), true)
		f.Add(raw, uint16(len(raw)-3), byte(0x80), true)
		f.Add(raw, uint16(3), byte(0xff), false)
	}
	f.Fuzz(func(t *testing.T, raw []byte, pos uint16, xor byte, sign bool) {
		if len(raw) > 0 {
			raw[int(pos)%len(raw)] ^= xor
		}
		if sign && len(raw) >= 4 {
			resignCompact(raw)
		}
		c, err := ParseCompactEndpoint(string(raw), cnfrt.VerifyUntrusted)
		if err != nil {
			return
		}
		_, _, _, _ = c.Name(), c.Port(), c.Weight(), c.Up()
		_, _, _, _ = c.Addr(), c.Prefix(), c.AP(), c.Seen()
		_, _ = c.Opt()
		_, _ = c.OptName()
		_, _ = c.Fee()
		_, _ = c.Arr(), c.Blob()
		v := c.View()
		for s := range v.Tags().All() {
			_ = s
		}
		_ = v.Meta().Key()
		_, _ = c.Unpack()
	})
}
