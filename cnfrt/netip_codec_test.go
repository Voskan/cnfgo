package cnfrt

import (
	"net/netip"
	"testing"
)

func TestAddrRoundTrip(t *testing.T) {
	var z ZoneTable
	addrs := []netip.Addr{
		{},
		netip.MustParseAddr("10.1.2.3"),
		netip.MustParseAddr("0.0.0.0"),
		netip.MustParseAddr("::"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("::ffff:10.1.2.3"),
		netip.MustParseAddr("fe80::1%eth0"),
		netip.MustParseAddr("fe80::1%eth1"),
		netip.MustParseAddr("fe80::1%eth0"),
	}
	var flats []FlatAddr
	for _, a := range addrs {
		flats = append(flats, z.EncodeAddr(a))
	}
	if len(z.Addrs) != 2 {
		t.Fatalf("zoned table has %d entries, want 2", len(z.Addrs))
	}
	for i, f := range flats {
		if !f.Valid(len(z.Addrs)) {
			t.Fatalf("%v invalid", addrs[i])
		}
		got := DecodeAddr(f, z.Addrs)
		if got != addrs[i] {
			t.Fatalf("round trip %v -> %v", addrs[i], got)
		}
		zone := ""
		if f.Kind == AddrV6Zoned {
			zone = addrs[i].Zone()
		}
		if got := DecodeAddrZone(f, zone); got != addrs[i] {
			t.Fatalf("zone round trip %v -> %v", addrs[i], got)
		}
	}
	zoned := flats[6]
	assertNoAllocs(t, "zoned decode", func() { _ = DecodeAddr(zoned, z.Addrs) })
	v4 := flats[1]
	assertNoAllocs(t, "v4 decode", func() { _ = DecodeAddr(v4, nil) })
}

func TestPrefixRoundTrip(t *testing.T) {
	addr4 := netip.MustParseAddr("192.168.1.77")
	addr6 := netip.MustParseAddr("2001:db8::77")
	prefixes := []netip.Prefix{
		{},
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("0.0.0.0/0"),
		netip.MustParsePrefix("192.168.1.77/32"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("::/0"),
		netip.MustParsePrefix("2001:db8::77/128"),
		netip.PrefixFrom(addr4, 33),
		netip.PrefixFrom(addr6, 200),
		netip.PrefixFrom(addr4, -1),
		netip.PrefixFrom(netip.MustParseAddr("fe80::1%eth0"), 64),
	}
	for _, p := range prefixes {
		f := EncodePrefix(p)
		if !f.Valid() {
			t.Fatalf("%v invalid", p)
		}
		got := DecodePrefix(f)
		if got != p {
			t.Fatalf("round trip %v (bits %d, addr %v) -> %v (bits %d, addr %v)", p, p.Bits(), p.Addr(), got, got.Bits(), got.Addr())
		}
	}
	if EncodePrefix(netip.PrefixFrom(addr4, 33)).IP != addr4.As16() {
		t.Fatal("invalid prefix address bytes were cleared")
	}
	if (FlatPrefix{Kind: AddrV4, BitsPlusOne: 34}).Valid() || (FlatPrefix{Kind: 3}).Valid() {
		t.Fatal("invalid flat prefix accepted")
	}
}

func TestAddrPortRoundTrip(t *testing.T) {
	var z ZoneTable
	for _, ap := range []netip.AddrPort{
		{},
		netip.MustParseAddrPort("1.2.3.4:443"),
		netip.MustParseAddrPort("[2001:db8::1]:65535"),
		netip.MustParseAddrPort("[fe80::1%eth0]:8080"),
		netip.AddrPortFrom(netip.Addr{}, 9),
	} {
		f := z.EncodeAddrPort(ap)
		if got := DecodeAddrPort(f, z.Addrs); got != ap {
			t.Fatalf("round trip %v -> %v", ap, got)
		}
	}
	if (FlatAddr{Kind: AddrV6Zoned, ZonedIdx: 2}).Valid(1) || (FlatAddr{Kind: 4}).Valid(9) || (FlatAddr{Kind: AddrV4, ZonedIdx: 1}).Valid(9) {
		t.Fatal("invalid flat addr accepted")
	}
}
