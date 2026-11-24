package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/Voskan/cnfgo/cnfrt"
)

func main() {
	peers := flag.Int("peers", 20000, "number of peers")
	dir := flag.String("dir", os.TempDir(), "directory for the .cnf file")
	flag.Parse()
	if err := run(os.Stdout, *peers, *dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// buildNetMap returns a deterministic network map with n peers.
func buildNetMap(n int) *NetMap {
	rng := rand.New(rand.NewPCG(1, 2))
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	users := map[int64]*UserProfile{}
	for i := range 50 {
		id := int64(i + 1)
		users[id] = &UserProfile{ID: id, LoginName: fmt.Sprintf("user%d@example.com", id), DisplayName: fmt.Sprintf("User %d", id), ProfilePicURL: "https://example.com/avatar.png"}
	}
	online := true
	node := func(i int) *Node {
		v4 := netip.AddrFrom4([4]byte{100, 64 + byte(i>>16), byte(i >> 8), byte(i)})
		v6 := netip.AddrFrom16([16]byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0, 14: byte(i >> 8), 15: byte(i)})
		seen := base.Add(-time.Duration(rng.IntN(3600)) * time.Second)
		n := &Node{
			ID:        NodeID(i + 1),
			StableID:  fmt.Sprintf("n%08xCNTRL", i),
			Name:      fmt.Sprintf("host-%d.example.ts.net.", i),
			User:      users[int64(i%50+1)],
			Key:       fmt.Sprintf("nodekey:%064x", i),
			KeyExpiry: base.Add(180 * 24 * time.Hour),
			Machine:   fmt.Sprintf("mkey:%064x", i*7),
			DiscoKey:  fmt.Sprintf("discokey:%064x", i*13),
			Addresses: []netip.Prefix{netip.PrefixFrom(v4, 32), netip.PrefixFrom(v6, 128)},
			Endpoints: []netip.AddrPort{
				netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(i)}), 41641),
				netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 168, 1, byte(i)}), 41641),
			},
			HomeDERP: int32(1 + i%20),
			Hostinfo: Hostinfo{
				OS: []string{"linux", "windows", "macOS", "iOS", "android"}[i%5], OSVersion: "1.2.3",
				Hostname: fmt.Sprintf("host-%d", i), GoVersion: "go1.27.3",
				Services: []Service{{Proto: "tcp", Port: 22, Description: "sshd"}},
			},
			Created:           base.Add(-time.Duration(i) * time.Minute),
			LastSeen:          &seen,
			MachineAuthorized: true,
			Tags:              []string{"tag:server", "tag:prod"},
			CapMap:            map[string][]string{"https://example.com/cap/ssh": nil},
		}
		n.AllowedIPs = n.Addresses
		if i%3 == 0 {
			n.Online = &online
		}
		if i%100 == 0 {
			n.PrimaryRoutes = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
			n.AllowedIPs = append(n.AllowedIPs, n.PrimaryRoutes...)
			n.Hostinfo.NetInfo = &NetInfo{PreferredDERP: 1, DERPLatency: map[string]float64{"1-v4": 0.012, "2-v4": 0.031}}
		}
		return n
	}
	nm := &NetMap{
		Self:   node(0),
		Users:  users,
		Domain: "example.ts.net",
		DNS: DNSConfig{
			Resolvers: []netip.AddrPort{netip.MustParseAddrPort("100.100.100.100:53")},
			Domains:   []string{"example.ts.net"}, MagicDNS: true,
			ExtraRecords: map[string]netip.Addr{"db.internal": netip.MustParseAddr("100.64.0.10")},
		},
		PacketFilter: []FilterRule{{
			SrcIPs:   []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")},
			DstPorts: []PortRange{{Net: netip.MustParsePrefix("0.0.0.0/0"), First: 0, Last: 65535}},
			IPProto:  []int32{6, 17},
		}},
		CollectedAt: base,
	}
	for i := 1; i <= n; i++ {
		nm.Peers = append(nm.Peers, node(i))
	}
	return nm
}

func run(w io.Writer, peers int, dir string) error {
	nm := buildNetMap(peers)
	start := time.Now()
	snap, err := FreezeNetMap(nm)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "froze %d peers (%d nodes, %d users) in %v: %.1f MiB of pointer-free tables\n",
		len(nm.Peers), snap.LenNode(), snap.LenUserProfile(), time.Since(start).Round(time.Microsecond), float64(snap.SizeBytes())/(1<<20))

	// Shared user profiles are stored once.
	root := snap.Root()
	p := root.Peers().At(peers - 1)
	fmt.Fprintf(w, "peer %s: user %s, %d addresses, home DERP %d\n", p.Name(), p.User().LoginName(), p.Addresses().Len(), p.HomeDERP())
	if u, ok := root.Users().Get(7); ok {
		fmt.Fprintf(w, "user 7: %s\n", u.DisplayName())
	}

	// Persist the snapshot and map it back: the cold start only verifies
	// the header; pages are faulted in on demand.
	path := filepath.Join(dir, "netmap.cnf")
	if err := WriteNetMapSnapshotAtomic(path, snap); err != nil {
		return err
	}
	defer os.Remove(path)
	start = time.Now()
	mapped, err := MapNetMapFile(path, cnfrt.VerifyTrustedHeader)
	if err != nil {
		return err
	}
	coldStart := time.Since(start)
	defer mapped.Close()
	err = mapped.WithRootFaultSafe(func(v NetMapView) error {
		last := v.Peers().At(peers - 1)
		fmt.Fprintf(w, "mapped peer %d expires %s, online=%v\n", last.ID(), last.KeyExpiry().Format(time.DateOnly), func() bool { o, _ := last.Online(); return o }())
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "cold start (mmap + header verification): %v\n", coldStart.Round(time.Microsecond))

	// Compact values: one pointer per node in an ordinary Go map.
	byID := make(map[NodeID]CompactNode, len(nm.Peers))
	total := 0
	for _, n := range nm.Peers {
		c, err := PackNode(n)
		if err != nil {
			return err
		}
		byID[n.ID] = c
		total += len(c.Raw())
	}
	c := byID[NodeID(peers+1)]
	fmt.Fprintf(w, "compact node %d: %s, key expiry %s; %d compact nodes hold %.1f MiB in %d strings\n",
		c.ID(), c.Name(), c.KeyExpiry().Format(time.DateOnly), len(byID), float64(total)/(1<<20), len(byID))
	return nil
}
