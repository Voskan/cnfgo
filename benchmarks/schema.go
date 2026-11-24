// Package benchmarks measures the memory, collector and access costs of
// cnfgo representations against ordinary Go objects on a reproducible
// synthetic data set. The cmd/report program runs every scenario in a fresh
// process and prints the results as Markdown tables.
package benchmarks

import "time"

//go:generate go run github.com/Voskan/cnfgo/cmd/cnfgo -type=Catalog -compact=Item

// Catalog is a product catalog indexed by item ID.
type Catalog struct {
	Version string
	Items   map[uint64]*Item `cnf:",hash"`
}

// Item is one catalog entry.
type Item struct {
	ID      uint64
	SKU     string
	Title   string
	Price   int64
	Stock   int32
	Active  bool
	Tags    []string
	Attrs   map[string]string
	Updated time.Time
	Dims    Dims
	Seller  *Seller
}

// Dims are package dimensions in centimetres.
type Dims struct {
	W, H, D float32
}

// Seller is shared by many items.
type Seller struct {
	ID      uint32
	Name    string
	Country string
}
