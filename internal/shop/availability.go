// Package shop holds the rules that decide what a nearby shopper can buy, and
// the batch work over a list: pick lists, store ranking, and the load to carry
// home. It is pure: everything arrives as data already fetched, so the rules
// are testable against recorded responses.
package shop

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
)

// Store is a store near the user, with its straight-line distance.
type Store struct {
	ID         string  `json:"store_id"`
	Name       string  `json:"name"`
	State      string  `json:"state,omitempty"`
	DistanceKM float64 `json:"distance_km"`
}

// Offer is an item's price at a store that genuinely sells it there.
type Offer struct {
	Store        Store
	Price        float64
	UnitPrice    float64 // per comparison unit; 0 when the item has none
	InStore      bool
	ClickCollect bool
}

// Exclusion explains why an item is excluded outright, whatever the store:
// "marketplace" for a third-party seller's listing, "inactive" for an item
// Bunnings has delisted. Empty means it may qualify store by store.
func Exclusion(r bunnings.Raw) string {
	if r.Str("sellername") != "" {
		return "marketplace"
	}
	for k := range r {
		if strings.HasPrefix(k, "productranges_") && slices.Contains(r.Strs(k), "Marketplace") {
			return "marketplace"
		}
	}
	if !strings.EqualFold(r.Str("isactive"), "true") {
		return "inactive"
	}
	return ""
}

// Offers returns the stores, in the order given, at which an item qualifies:
// fulfilment there includes In-Store or Click & Collect and the store's price is
// positive. A price is never reported for any other store. Bunnings keeps stale
// prices against stores that do not range an item (a WA-only cement shows
// $1.54 in Melbourne), and that number is meaningless.
func Offers(r bunnings.Raw, stores []Store) []Offer {
	if Exclusion(r) != "" {
		return nil
	}
	var out []Offer
	for _, s := range stores {
		ful := r.Strs("fulfilmentoptions_" + s.ID)
		in, cnc := slices.Contains(ful, "In-Store"), slices.Contains(ful, "Click & Collect")
		price, ok := r.Num("price_" + s.ID)
		if !(in || cnc) || !ok || price <= 0 {
			continue
		}
		unit, _ := r.Num("cprice_" + s.ID)
		out = append(out, Offer{Store: s, Price: price, UnitPrice: unit, InStore: in, ClickCollect: cnc})
	}
	return out
}

// SearchFields lists the raw fields a search must request so Offers and
// Exclusion can be applied: the shared ones plus three per store.
func SearchFields(stores []Store, region string) []string {
	f := []string{
		"itemnumber", "name", "brandname", "productroutingurl", "rating", "ratingcount",
		"isactive", "sellername", "availableinregions", "unitofprice",
		"comparisonunit", "comparisonunitofmeasure",
		"promotionalcampaign", "promotionalcampaignend",
	}
	if region != "" {
		f = append(f, "productranges_"+strings.ToLower(region))
	}
	for _, s := range stores {
		f = append(f, "price_"+s.ID, "fulfilmentoptions_"+s.ID, "cprice_"+s.ID, "productranges_"+s.ID)
	}
	return f
}

// AvailabilityQuery is the Coveo advanced query that narrows a search to items
// some store in the list might sell in person. It is a first cut on the server
// so results are not wasted on delivery-only lines; Offers is still the rule.
func AvailabilityQuery(stores []Store) string {
	ors := make([]string, len(stores))
	for i, s := range stores {
		ors[i] = fmt.Sprintf(`@fulfilmentoptions_%s==("In-Store","Click & Collect")`, s.ID)
	}
	return "@isactive==true AND @batchcountry==(AU) AND NOT @sellername AND (" + strings.Join(ors, " OR ") + ")"
}

// ItemsQuery restricts a search to the given item numbers.
func ItemsQuery(items []string) string {
	return "@itemnumber==(" + strings.Join(items, ",") + ")"
}

// URL is the product page for a raw hit.
func URL(r bunnings.Raw) string {
	if p := r.Str("productroutingurl"); p != "" {
		return "https://www.bunnings.com.au" + p
	}
	return ""
}
