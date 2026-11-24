package cnfrt

import "net/netip"

// Address kinds stored in FlatAddr.Kind and FlatPrefix.Kind.
const (
	AddrInvalid = 0
	AddrV4      = 1
	AddrV6      = 2
	AddrV6Zoned = 3
)

// FlatAddr is the 20-byte pointer-free encoding of a netip.Addr.
//
// Unzoned addresses are rebuilt on the stack with netip.AddrFrom4 or
// netip.AddrFrom16. Zoned IPv6 addresses (Kind == AddrV6Zoned) reference a
// 1-based entry of the snapshot's zoned address table, which holds the fully
// built netip.Addr, so reads never call Addr.WithZone.
type FlatAddr struct {
	IP       [16]byte
	ZonedIdx uint16
	Kind     uint8
	Pad      uint8
}

// FlatPrefix is the 20-byte pointer-free encoding of a netip.Prefix.
// BitsPlusOne is Prefix.Bits()+1, so 0 encodes an invalid prefix; the
// address bytes are preserved even for invalid prefixes.
type FlatPrefix struct {
	IP          [16]byte
	BitsPlusOne uint8
	Kind        uint8
	Pad         [2]byte
}

// FlatAddrPort is the 24-byte pointer-free encoding of a netip.AddrPort.
type FlatAddrPort struct {
	Addr FlatAddr
	Port uint16
	Pad  [2]byte
}

// ZoneTable deduplicates zoned IPv6 addresses while freezing.
type ZoneTable struct {
	Addrs []netip.Addr
	index map[netip.Addr]uint16
}

// EncodeAddr converts a to its flat form, registering zoned addresses in
// the table.
func (z *ZoneTable) EncodeAddr(a netip.Addr) FlatAddr {
	var f FlatAddr
	switch {
	case !a.IsValid():
		return f
	case a.Is4():
		f.IP = a.As16()
		f.Kind = AddrV4
	case a.Zone() == "":
		f.IP = a.As16()
		f.Kind = AddrV6
	default:
		f.IP = a.As16()
		f.Kind = AddrV6Zoned
		idx, ok := z.index[a]
		if !ok {
			if len(z.Addrs) >= 1<<16-1 {
				Fail(ErrCapacityExceeded)
			}
			if z.index == nil {
				z.index = make(map[netip.Addr]uint16)
			}
			z.Addrs = append(z.Addrs, a)
			idx = uint16(len(z.Addrs))
			z.index[a] = idx
		}
		f.ZonedIdx = idx
	}
	return f
}

// EncodeAddrPort converts ap to its flat form.
func (z *ZoneTable) EncodeAddrPort(ap netip.AddrPort) FlatAddrPort {
	return FlatAddrPort{Addr: z.EncodeAddr(ap.Addr()), Port: ap.Port()}
}

// DecodeAddr converts f back to a netip.Addr. zoned is the snapshot's zoned
// address table. It does not allocate.
func DecodeAddr(f FlatAddr, zoned []netip.Addr) netip.Addr {
	switch f.Kind {
	case AddrV4:
		return netip.AddrFrom4([4]byte(f.IP[12:16]))
	case AddrV6:
		return netip.AddrFrom16(f.IP)
	case AddrV6Zoned:
		return zoned[f.ZonedIdx-1]
	}
	return netip.Addr{}
}

// DecodeAddrZone converts f back to a netip.Addr using an explicit zone
// name for zoned addresses. It is used by Compact values, which carry zone
// names inline instead of a zoned address table.
func DecodeAddrZone(f FlatAddr, zone string) netip.Addr {
	if f.Kind == AddrV6Zoned {
		return netip.AddrFrom16(f.IP).WithZone(zone)
	}
	return DecodeAddr(f, nil)
}

// Valid reports whether f is structurally valid for a zoned address table
// of length nzoned.
func (f FlatAddr) Valid(nzoned int) bool {
	switch f.Kind {
	case AddrInvalid, AddrV4, AddrV6:
		return f.ZonedIdx == 0
	case AddrV6Zoned:
		return f.ZonedIdx != 0 && int(f.ZonedIdx) <= nzoned
	}
	return false
}

// DecodeAddrPort converts f back to a netip.AddrPort.
func DecodeAddrPort(f FlatAddrPort, zoned []netip.Addr) netip.AddrPort {
	return netip.AddrPortFrom(DecodeAddr(f.Addr, zoned), f.Port)
}

// EncodePrefix converts p to its flat form.
func EncodePrefix(p netip.Prefix) FlatPrefix {
	var f FlatPrefix
	a := p.Addr()
	switch {
	case !a.IsValid():
		return f
	case a.Is4():
		f.Kind = AddrV4
	default:
		f.Kind = AddrV6
	}
	f.IP = a.As16()
	f.BitsPlusOne = uint8(p.Bits() + 1)
	return f
}

// DecodePrefix converts f back to a netip.Prefix. Invalid prefixes keep
// their address, matching netip.PrefixFrom semantics.
func DecodePrefix(f FlatPrefix) netip.Prefix {
	bits := int(f.BitsPlusOne) - 1
	switch f.Kind {
	case AddrV4:
		return netip.PrefixFrom(netip.AddrFrom4([4]byte(f.IP[12:16])), bits)
	case AddrV6:
		return netip.PrefixFrom(netip.AddrFrom16(f.IP), bits)
	}
	return netip.Prefix{}
}

// Valid reports whether f is structurally valid.
func (f FlatPrefix) Valid() bool {
	switch f.Kind {
	case AddrInvalid:
		return f.BitsPlusOne == 0
	case AddrV4:
		return f.BitsPlusOne <= 33
	case AddrV6:
		return f.BitsPlusOne <= 129
	}
	return false
}
