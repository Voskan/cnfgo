// Package unsafemmap maps .cnf snapshot files into memory read-only and
// exposes their sections as typed slices without copying.
//
// Files are mapped with MAP_PRIVATE and PROT_READ, so a mapped snapshot can
// never be modified through the mapping and writes by other processes after
// an atomic rename (see cnfrt.WriteSnapshotAtomic) are never observed. A
// Mapping is released by Close, which is idempotent, or by a cleanup that
// runs once its owner becomes unreachable. Memory faults caused by files
// truncated underneath a mapping can be converted into cnfrt.ErrMmapFault
// with FaultSafe.
//
// This package and cnfrt are the only packages of the module that use
// package unsafe; generated code calls them instead.
package unsafemmap
