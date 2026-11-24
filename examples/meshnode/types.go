// Command meshnode freezes the network map of a mesh VPN control plane: a
// graph of nodes with addresses, routes, endpoints, keys with expiry times
// and shared user profiles. It demonstrates snapshots, Compact node values
// and memory-mapped .cnf files.
package main

import (
	"net/netip"
	"time"
)

//go:generate go run github.com/Voskan/cnfgo/cmd/cnfgo -type=NetMap -compact=Node

// NodeID identifies a node.
type NodeID int64

// NetMap is the network map sent to one node: itself, its peers and the
// shared configuration.
type NetMap struct {
	Self         *Node
	Peers        []*Node
	Users        map[int64]*UserProfile `cnf:",hash"`
	DNS          DNSConfig
	PacketFilter []FilterRule
	Domain       string
	CollectedAt  time.Time
}

// Node is a device in the mesh.
type Node struct {
	ID                NodeID
	StableID          string
	Name              string
	User              *UserProfile
	Key               string
	KeyExpiry         time.Time
	Machine           string
	DiscoKey          string
	Addresses         []netip.Prefix
	AllowedIPs        []netip.Prefix
	PrimaryRoutes     []netip.Prefix
	Endpoints         []netip.AddrPort
	HomeDERP          int32
	Hostinfo          Hostinfo
	Created           time.Time
	LastSeen          *time.Time
	Online            *bool
	MachineAuthorized bool
	Expired           bool
	Tags              []string
	CapMap            map[string][]string
}

// Hostinfo describes the host running a node.
type Hostinfo struct {
	OS        string
	OSVersion string
	Hostname  string
	GoVersion string
	Services  []Service
	NetInfo   *NetInfo
}

// Service is a listening service advertised by a host.
type Service struct {
	Proto       string
	Port        uint16
	Description string
}

// NetInfo describes the network conditions of a host.
type NetInfo struct {
	MappingVariesByDestIP *bool
	PreferredDERP         int32
	DERPLatency           map[string]float64
}

// UserProfile is shared by every node of a user.
type UserProfile struct {
	ID            int64
	LoginName     string
	DisplayName   string
	ProfilePicURL string
}

// DNSConfig is the DNS configuration of the network.
type DNSConfig struct {
	Resolvers    []netip.AddrPort
	Domains      []string
	MagicDNS     bool
	ExtraRecords map[string]netip.Addr
}

// FilterRule allows traffic from source prefixes to destination ports.
type FilterRule struct {
	SrcIPs   []netip.Prefix
	DstPorts []PortRange
	IPProto  []int32
}

// PortRange is a destination prefix with an inclusive port range.
type PortRange struct {
	Net   netip.Prefix
	First uint16
	Last  uint16
}
