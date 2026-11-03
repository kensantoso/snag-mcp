package shop

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
)

// Line statuses. These are machine names; the client phrases them.
const (
	InStock      = "in_stock"      // stock covers the quantity
	Short        = "short"         // some stock, less than the quantity
	OutOfStock   = "out_of_stock"  // sold here, none on hand
	SpecialOrder = "special_order" // ordered in store; there is no shelf stock to count
	StockUnknown = "stock_unknown" // sold here, but the store gave no count
	NotSoldHere  = "not_sold_here"
	Marketplace  = "marketplace" // third-party seller, delivery only; never in store
	Inactive     = "inactive"    // delisted
	NotFound     = "not_found"   // no such item number in the Australian range
)

// Check is everything fetched about one item at one store.
type Check struct {
	Raw   bunnings.Raw // the item's search hit; nil when not found
	Offer *Offer       // nil when the item does not qualify at this store
	// Checked is set once the store's live record has been read; until then
	// stock is unknown.
	Checked bool
	Stock   *int // exact count, nil when the store reported none
	Special bool // special order: ordered in store, no shelf stock to count
	// StoreSaysNo is the store's own record overruling the search index:
	// inactive, a marketplace link, or not ranged there.
	StoreSaysNo bool
	Locations   []bunnings.Location
}

// FromFulfilment fills a check from the single-store fulfilment lookup.
func (c Check) FromFulfilment(f *bunnings.Fulfilment) Check {
	if f == nil {
		return c
	}
	c.Checked, c.Stock, c.Special = true, f.InStoreStock, f.IsSpecial
	c.StoreSaysNo = !f.IsActive || f.IsMarketLink
	return c
}

// FromStockNear fills a check from the multi-store stock lookup for one
// store. found is whether the lookup had the item there. That lookup cannot
// tell a special order from stock, and the product record that can is read
// for one store only (the nearest). Its flag, specialThere, is applied only
// where this store reports no count of its own: a real count elsewhere is
// shelf stock, and treating it as a special order would hide it.
func (c Check) FromStockNear(found bool, s bunnings.StoreStock, specialThere bool) Check {
	if found {
		c.Checked, c.Stock, c.StoreSaysNo = true, s.Level, !s.Visible
	}
	c.Special = specialThere && c.Stock == nil
	return c
}

// Line is one item on a store's list.
type Line struct {
	ItemNumber string   `json:"item_number"`
	Name       string   `json:"name,omitempty"`
	Quantity   int      `json:"quantity"`
	Status     string   `json:"status"`
	Stock      *int     `json:"stock,omitempty"`
	UnitPrice  *float64 `json:"unit_price,omitempty"`
	LineTotal  *float64 `json:"line_total,omitempty"`
	Aisle      string   `json:"aisle,omitempty"`
	Bay        string   `json:"bay,omitempty"`
	// OtherLocations lists further aisle/bay spots when the store keeps the
	// item in more than one place.
	OtherLocations []string `json:"other_locations,omitempty"`
	// Stop is the line's number on the store map, in walking order from the
	// entrance; lines in the same bay share one. Zero when not on the map.
	Stop int `json:"stop,omitempty"`
}

// MakeLine decides the status of one item at one store.
func MakeLine(item string, qty int, c Check) Line {
	l := Line{ItemNumber: item, Quantity: qty}
	if c.Raw == nil {
		l.Status = NotFound
		return l
	}
	l.Name = strings.TrimSpace(c.Raw.Str("name"))
	switch Exclusion(c.Raw) {
	case "marketplace":
		l.Status = Marketplace
		return l
	case "inactive":
		l.Status = Inactive
		return l
	}
	if c.Offer == nil || c.StoreSaysNo {
		l.Status = NotSoldHere
		return l
	}
	price := c.Offer.Price
	total := math.Round(price*float64(qty)*100) / 100
	l.UnitPrice, l.LineTotal = &price, &total
	if len(c.Locations) > 0 {
		l.Aisle, l.Bay = c.Locations[0].Aisle, c.Locations[0].Bay
		for _, o := range c.Locations[1:] {
			s := "aisle " + o.Aisle + " bay " + o.Bay
			// Stores list the same spot more than once (one per facing).
			if o != c.Locations[0] && !slices.Contains(l.OtherLocations, s) {
				l.OtherLocations = append(l.OtherLocations, s)
			}
		}
	}
	switch {
	case c.Special:
		l.Status = SpecialOrder
	case !c.Checked || c.Stock == nil:
		l.Status = StockUnknown
	default:
		l.Stock = c.Stock
		switch s := *c.Stock; {
		case s >= qty:
			l.Status = InStock
		case s > 0:
			l.Status = Short
		default:
			l.Status = OutOfStock
		}
	}
	return l
}

// Sold reports whether the line is sold at the store, whatever its stock.
func (l Line) Sold() bool { return l.UnitPrice != nil }

// SortPickList orders lines for walking the store: by aisle then bay, numbers
// numerically ("9" before "10"), named aisles after numbered ones, and lines
// with no location last. Lines not sold at the store go to the very end.
func SortPickList(lines []Line) {
	slices.SortStableFunc(lines, func(a, b Line) int {
		if a.Sold() != b.Sold() {
			if a.Sold() {
				return -1
			}
			return 1
		}
		if c := compareLoc(a.Aisle, b.Aisle); c != 0 {
			return c
		}
		return compareLoc(a.Bay, b.Bay)
	})
}

func compareLoc(a, b string) int {
	if (a == "") != (b == "") {
		if a == "" {
			return 1
		}
		return -1
	}
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmp.Compare(an, bn)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// Total sums line totals over lines sold at the store.
func Total(lines []Line) float64 {
	var t float64
	for _, l := range lines {
		if l.LineTotal != nil {
			t += *l.LineTotal
		}
	}
	return math.Round(t*100) / 100
}

// Missing is a line a store cannot fill from stock, and why.
type Missing struct {
	ItemNumber string `json:"item_number"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status"`
	Stock      *int   `json:"stock,omitempty"`
	Needed     int    `json:"needed"`
}

// Ranked is one store's ability to fill the list.
type Ranked struct {
	Store
	Fillable int `json:"lines_in_stock"`
	Of       int `json:"lines_total"`
	// CostInStock is what the lines this store can fill from stock cost here.
	CostInStock float64   `json:"cost_of_lines_in_stock"`
	Complete    bool      `json:"fills_whole_list"`
	Missing     []Missing `json:"missing,omitempty"`
}

// Rank orders stores by how many lines each fills from stock, nearest first
// among equals. A line counts only when the quantity is on hand: a store with
// three of the ten lengths you need does not fill that line.
func Rank(stores []Store, lines map[string][]Line) []Ranked {
	out := make([]Ranked, 0, len(stores))
	for _, s := range stores {
		r := Ranked{Store: s}
		for _, l := range lines[s.ID] {
			r.Of++
			if l.Status == InStock {
				r.Fillable++
				r.CostInStock += *l.LineTotal
				continue
			}
			r.Missing = append(r.Missing, Missing{
				ItemNumber: l.ItemNumber, Name: l.Name, Status: l.Status, Stock: l.Stock, Needed: l.Quantity,
			})
		}
		r.CostInStock = math.Round(r.CostInStock*100) / 100
		r.Complete = r.Of > 0 && r.Fillable == r.Of
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b Ranked) int {
		if c := cmp.Compare(b.Fillable, a.Fillable); c != 0 {
			return c
		}
		return cmp.Compare(a.DistanceKM, b.DistanceKM)
	})
	return out
}
