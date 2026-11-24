package benchmarks

import (
	"fmt"
	"time"
	"unique"
)

// NumSellers is the number of distinct sellers in a generated catalog.
const NumSellers = 1000

// MakeCatalog returns a deterministic catalog of n items with IDs 1..n.
func MakeCatalog(n int) *Catalog {
	return makeCatalog(n, false, false)
}

// MakeHighCardCatalog returns a deterministic catalog of n items in which
// every item has both a unique SKU and a unique Title (zero title sharing).
func MakeHighCardCatalog(n int) *Catalog {
	return makeCatalog(n, true, false)
}

// MakeInternedCatalog returns MakeCatalog(n) with every repeated string
// canonicalized via the standard library's unique.Make[string].
func MakeInternedCatalog(n int) *Catalog {
	return makeCatalog(n, false, true)
}

func internStr(s string, on bool) string {
	if !on {
		return s
	}
	return unique.Make(s).Value()
}

func makeCatalog(n int, highCard, intern bool) *Catalog {
	sellers := make([]*Seller, NumSellers)
	countries := []string{"DE", "US", "JP", "BR", "IN", "FR", "KR", "CA"}
	for i := range sellers {
		sellers[i] = &Seller{
			ID:      uint32(i + 1),
			Name:    internStr(fmt.Sprintf("Seller %04d GmbH", i), intern),
			Country: internStr(countries[i%len(countries)], intern),
		}
	}
	c := &Catalog{Version: "2026-11-22", Items: make(map[uint64]*Item, n)}
	for i := range n {
		it := makeItem(uint64(i+1), sellers[i%NumSellers], highCard, intern)
		c.Items[it.ID] = it
	}
	return c
}

// MakeItem returns the item with the given ID.
func MakeItem(id uint64, seller *Seller) *Item {
	return makeItem(id, seller, false, false)
}

func makeItem(id uint64, seller *Seller, highCard, intern bool) *Item {
	var title string
	if highCard {
		title = fmt.Sprintf("Industrial sensor module #%010d rev-%08x", id, uint32(id*0x9e3779b1))
	} else {
		title = internStr(fmt.Sprintf("Stainless steel water bottle, %d ml, model %d", 250+id%8*125, id%977), intern)
	}
	return &Item{
		ID:      id,
		SKU:     fmt.Sprintf("SKU-%010d", id),
		Title:   title,
		Price:   int64(499 + id%10_000),
		Stock:   int32(id % 1000),
		Active:  id%11 != 0,
		Tags:    []string{internStr("kitchen", intern), internStr("outdoor", intern), internStr(fmt.Sprintf("batch-%d", id%64), intern)},
		Attrs:   map[string]string{internStr("color", intern): internStr([]string{"black", "white", "steel", "blue"}[id%4], intern), internStr("material", intern): internStr("steel", intern)},
		Updated: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Minute),
		Dims:    Dims{W: 7.5, H: 26, D: 7.5},
		Seller:  seller,
	}
}
