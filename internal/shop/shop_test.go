package shop

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
)

// reference_items.json is an invented search answer in Coveo's shape, with
// fields for four made-up stores: Northfield (0101) and Southvale (0102) in
// Victoria, Westhaven (0201) in WA and Topend (0301) in the NT. Its items are
// the traps the availability rule exists for: pine priced at Topend but only
// delivered there, a WA-only cement with a stale price in Victoria, a pine
// ranged as a special order, and a marketplace listing.
func reference(t *testing.T) map[string]bunnings.Raw {
	t.Helper()
	b, err := os.ReadFile("testdata/reference_items.json")
	if err != nil {
		t.Fatal(err)
	}
	var raws []bunnings.Raw
	if err := json.Unmarshal(b, &raws); err != nil {
		t.Fatal(err)
	}
	out := map[string]bunnings.Raw{}
	for _, r := range raws {
		out[r.Str("itemnumber")] = r
	}
	return out
}

var (
	northfield = Store{ID: "0101", Name: "Northfield", DistanceKM: 0.4}
	southvale  = Store{ID: "0102", Name: "Southvale", DistanceKM: 4}
	westhaven  = Store{ID: "0201", Name: "Westhaven", DistanceKM: 3.8}
	topend     = Store{ID: "0301", Name: "Topend", DistanceKM: 5}
	all        = []Store{northfield, southvale, westhaven, topend}
)

func offerIDs(os []Offer) map[string]float64 {
	m := map[string]float64{}
	for _, o := range os {
		m[o.Store.ID] = o.Price
	}
	return m
}

// The WA-only cement carries a $1.54 price against Victorian stores that do
// not range it, as real WA-only lines do. Reporting that would be the worst
// kind of wrong answer: cheap, specific, and unbuyable.
func TestWAOnlyCementOnlyQualifiesInWA(t *testing.T) {
	got := offerIDs(Offers(reference(t)["9100003"], all))
	if len(got) != 1 || got["0201"] != 13.22 {
		t.Fatalf("offers = %v, want only Westhaven at 13.22", got)
	}
}

func TestMarketplaceItemNeverQualifies(t *testing.T) {
	r := reference(t)["9100005"]
	if got := Exclusion(r); got != "marketplace" {
		t.Fatalf("Exclusion = %q, want marketplace", got)
	}
	if got := Offers(r, all); len(got) != 0 {
		t.Fatalf("offers = %v, want none", offerIDs(got))
	}
	// Seller name alone is enough, as is a Marketplace range with no seller.
	sellerOnly := bunnings.Raw{}
	for k, v := range r {
		if !strings.HasPrefix(k, "productranges_") {
			sellerOnly[k] = v
		}
	}
	if Exclusion(sellerOnly) != "marketplace" {
		t.Error("a seller's listing without a Marketplace range was not excluded")
	}
	noSeller := bunnings.Raw{}
	for k, v := range r {
		if k != "sellername" {
			noSeller[k] = v
		}
	}
	if Exclusion(noSeller) != "marketplace" {
		t.Error("a Marketplace product range without a seller name was not excluded")
	}
}

// Topend has a price for the 3.0 m pine but only delivers it, so it does not
// sell it in the sense that matters, while In-Store alone (no Click & Collect)
// still counts at Northfield.
func TestPineQualifiesStoreByStore(t *testing.T) {
	got := offerIDs(Offers(reference(t)["9100001"], all))
	want := map[string]float64{"0101": 23.13, "0102": 23.13, "0201": 27.57}
	if len(got) != len(want) {
		t.Fatalf("offers = %v, want %v", got, want)
	}
	for id, p := range want {
		if got[id] != p {
			t.Errorf("store %s price = %v, want %v", id, got[id], p)
		}
	}
}

func TestInactiveExcluded(t *testing.T) {
	r := bunnings.Raw{"isactive": json.RawMessage(`"false"`), "price_0101": json.RawMessage(`5`),
		"fulfilmentoptions_0101": json.RawMessage(`["In-Store"]`)}
	if Exclusion(r) != "inactive" || len(Offers(r, all)) != 0 {
		t.Fatal("an inactive item qualified")
	}
}

func intp(i int) *int { return &i }

func TestMakeLineStatuses(t *testing.T) {
	ref := reference(t)
	pine := ref["9100001"]
	offer := &Offers(pine, []Store{northfield})[0]
	base := Check{Raw: pine, Offer: offer}
	ful := func(f bunnings.Fulfilment) Check { return base.FromFulfilment(&f) }
	for _, c := range []struct {
		name string
		qty  int
		chk  Check
		want string
	}{
		{"enough", 10, ful(bunnings.Fulfilment{IsActive: true, InStoreStock: intp(10)}), InStock},
		{"one short", 10, ful(bunnings.Fulfilment{IsActive: true, InStoreStock: intp(9)}), Short},
		{"none", 1, ful(bunnings.Fulfilment{IsActive: true, InStoreStock: intp(0)}), OutOfStock},
		{"no count", 1, ful(bunnings.Fulfilment{IsActive: true}), StockUnknown},
		{"not looked up", 1, base, StockUnknown},
		{"special order", 1, ful(bunnings.Fulfilment{IsActive: true, IsSpecial: true}), SpecialOrder},
		// The search index says sold; the store's own record says inactive,
		// or that it is a marketplace link. The store wins.
		{"store says inactive", 1, ful(bunnings.Fulfilment{IsActive: false, InStoreStock: intp(5)}), NotSoldHere},
		{"store says marketplace", 1, ful(bunnings.Fulfilment{IsActive: true, IsMarketLink: true, InStoreStock: intp(5)}), NotSoldHere},
		{"no offer", 1, Check{Raw: ref["9100003"], Checked: true, Stock: intp(16)}, NotSoldHere},
		{"marketplace", 1, Check{Raw: ref["9100005"]}, Marketplace},
		{"unknown item", 1, Check{}, NotFound},
	} {
		l := MakeLine("x", c.qty, c.chk)
		if l.Status != c.want {
			t.Errorf("%s: status %q, want %q", c.name, l.Status, c.want)
		}
		if (l.UnitPrice != nil) != (c.want != NotSoldHere && c.want != Marketplace && c.want != NotFound) {
			t.Errorf("%s: price reported = %v for status %q", c.name, l.UnitPrice != nil, l.Status)
		}
	}
	c := ful(bunnings.Fulfilment{IsActive: true, InStoreStock: intp(50)})
	c.Locations = []bunnings.Location{{Aisle: "403", Bay: "3"}, {Aisle: "403", Bay: "3"}, {Aisle: "12", Bay: "1"}, {Aisle: "12", Bay: "1"}}
	l := MakeLine("9100001", 3, c)
	if *l.LineTotal != 69.39 {
		t.Errorf("3 x 23.13 = %v, want 69.39", *l.LineTotal)
	}
	// The pine is listed at aisle 403 bay 3 twice; that is one place.
	if l.Aisle != "403" || l.Bay != "3" || len(l.OtherLocations) != 1 || l.OtherLocations[0] != "aisle 12 bay 1" {
		t.Errorf("location %s/%s, others %v", l.Aisle, l.Bay, l.OtherLocations)
	}
}

// An item the product record calls a special order where it was read (as
// long timber can be at one store) is on the shelf at a store that reports a
// count of its own.
func TestSpecialOrderIsPerStore(t *testing.T) {
	ref := reference(t)
	pine := ref["9100004"]
	base := Check{Raw: pine, Offer: &Offers(pine, []Store{northfield})[0]}
	for _, c := range []struct {
		name  string
		found bool
		stock bunnings.StoreStock
		want  string
	}{
		{"counted elsewhere", true, bunnings.StoreStock{Level: intp(12), Visible: true}, InStock},
		{"counted none elsewhere", true, bunnings.StoreStock{Level: intp(0), Visible: true}, OutOfStock},
		{"no count here either", true, bunnings.StoreStock{Visible: true}, SpecialOrder},
		{"not in the lookup", false, bunnings.StoreStock{}, SpecialOrder},
	} {
		if got := MakeLine("x", 2, base.FromStockNear(c.found, c.stock, true)).Status; got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// Not special where the record was read: counts as before.
	if got := MakeLine("x", 2, base.FromStockNear(true, bunnings.StoreStock{Visible: true}, false)).Status; got != StockUnknown {
		t.Errorf("no count, not special: %s", got)
	}
}

func TestSortPickList(t *testing.T) {
	p := 1.0
	sold := func(aisle, bay string) Line {
		return Line{ItemNumber: aisle + "/" + bay, Aisle: aisle, Bay: bay, UnitPrice: &p}
	}
	lines := []Line{
		{ItemNumber: "unsold", Aisle: "1", Bay: "1"},
		sold("", ""),
		sold("22", "2"),
		sold("TIMBER", "1"),
		sold("9", "10"),
		sold("10", "1"),
		sold("9", "9"),
	}
	SortPickList(lines)
	want := []string{"9/9", "9/10", "10/1", "22/2", "TIMBER/1", "/", "unsold"}
	for i, l := range lines {
		if l.ItemNumber != want[i] {
			got := make([]string, len(lines))
			for j, x := range lines {
				got[j] = x.ItemNumber
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestRankPrefersFillingOverNearness(t *testing.T) {
	p := 10.0
	line := func(status string) Line { return Line{Status: status, Quantity: 1, UnitPrice: &p, LineTotal: &p} }
	near := Store{ID: "a", DistanceKM: 1}
	mid := Store{ID: "b", DistanceKM: 5}
	far := Store{ID: "c", DistanceKM: 20}
	got := Rank([]Store{near, mid, far}, map[string][]Line{
		"a": {line(InStock), line(Short)},
		"b": {line(InStock), line(OutOfStock)},
		"c": {line(InStock), line(InStock)},
	})
	if got[0].ID != "c" || !got[0].Complete || got[0].CostInStock != 20 {
		t.Fatalf("first = %+v, want the far store that fills everything", got[0])
	}
	if got[1].ID != "a" || got[2].ID != "b" {
		t.Errorf("tie order = %s, %s; want nearest first", got[1].ID, got[2].ID)
	}
	if len(got[1].Missing) != 1 || got[1].Missing[0].Status != Short {
		t.Errorf("missing = %+v, want the short line", got[1].Missing)
	}
}

func TestParseLengthMM(t *testing.T) {
	for in, want := range map[string]float64{
		"4.8 mtr": 4800, "0.045 mtr": 45, "3 mtr": 3000, "450 mm": 450, "45cm": 450, "2.4m": 2400,
	} {
		if got, ok := ParseLengthMM(in); !ok || got != want {
			t.Errorf("ParseLengthMM(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "12 in", "mm", "-3 mm", "0 mm"} {
		if got, ok := ParseLengthMM(in); ok {
			t.Errorf("ParseLengthMM(%q) = %v, want unknown", in, got)
		}
	}
}

func TestLoadSummary(t *testing.T) {
	// A 4.8 m length of pine's package as the product API gives it: length,
	// width, height in metres.
	h4 := Packages([][3]string{{"4.8 mtr", "0.045 mtr", "0.09 mtr"}})
	if len(h4) != 1 || h4[0] != (Package{4800, 90, 45}) {
		t.Fatalf("packages = %+v", h4)
	}
	// Some products list the same package many times over.
	dup := [][3]string{}
	for range 16 {
		dup = append(dup, [3]string{"0.045 mtr", "3 mtr", "0.09 mtr"})
	}
	pine := Packages(dup)
	if len(pine) != 1 || pine[0].LengthMM != 3000 {
		t.Fatalf("duplicate packages = %+v", pine)
	}

	w := ParseWeightKG(json.RawMessage(`"9.72"`))
	l := Summarise([]LoadItem{
		{Item: "9100004", Name: "H4 pine 4.8m", Qty: 3, WeightKG: w, Packages: h4},
		{Item: "9100001", Name: "H3 pine 3.0m", Qty: 10, WeightKG: ParseWeightKG(nil), Packages: pine},
		{Item: "9100002", Name: "Drill bits", Qty: 1, WeightKG: ParseWeightKG(json.RawMessage(`0.88`))},
	})
	if l.Longest == nil || l.Longest.ItemNumber != "9100004" || l.Longest.LengthM != 4.8 {
		t.Errorf("longest = %+v", l.Longest)
	}
	if l.KnownWeightKG != 30.04 { // 3 x 9.72 + 0.88; the 3.0 m pine is unknown, not zero
		t.Errorf("known weight = %v, want 30.04", l.KnownWeightKG)
	}
	if len(l.WeightUnknown) != 1 || l.WeightUnknown[0] != "H3 pine 3.0m" {
		t.Errorf("weight unknown = %v", l.WeightUnknown)
	}
	if len(l.SizeUnknown) != 1 || l.SizeUnknown[0] != "Drill bits" {
		t.Errorf("size unknown = %v", l.SizeUnknown)
	}
	for _, raw := range []string{`""`, `"0"`, `0`, `null`, `"heavy"`} {
		if ParseWeightKG(json.RawMessage(raw)) != nil {
			t.Errorf("ParseWeightKG(%s) should be unknown", raw)
		}
	}
}
