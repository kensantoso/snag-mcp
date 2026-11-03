package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type getProductIn struct {
	ItemNumber string  `json:"item_number" jsonschema:"Bunnings item number from search_products, e.g. '0563516'. Keep leading zeros."`
	Location   string  `json:"location,omitempty" jsonschema:"Postcode or suburb for this call only. Omit to use the saved location."`
	RadiusKM   float64 `json:"radius_km,omitempty" jsonschema:"How far to look for a store that sells it, in km. Default 50."`
}

type nearbyPrice struct {
	SoldNearby bool `json:"sold_nearby"`
	// Reason is set when it is not: not_sold_nearby, marketplace, inactive or
	// not_found.
	Reason        string      `json:"reason,omitempty"`
	StoresSelling int         `json:"stores_selling"`
	Nearest       *storePrice `json:"nearest_store,omitempty"`
	UnitPrice     *unitPrice  `json:"unit_price,omitempty"`
	PriceMin      float64     `json:"price_min,omitempty"`
	PriceMax      float64     `json:"price_max,omitempty"`
}

type getProductOut struct {
	ItemNumber  string      `json:"item_number"`
	Name        string      `json:"name"`
	Brand       string      `json:"brand,omitempty"`
	URL         string      `json:"url,omitempty"`
	Description string      `json:"description,omitempty"`
	KeyPoints   []string    `json:"key_selling_points,omitempty"`
	Specs       []shop.Spec `json:"specs,omitempty"`
	// ConnectionDetails collects every statement Bunnings makes about how
	// the item connects, verbatim, from specs, name, key points and
	// description. ConnectionNote says what that does and does not establish.
	ConnectionDetails []shop.ConnectionDetail `json:"connection_details"`
	ConnectionNote    string                  `json:"connection_note"`
	Documents         []shop.Document         `json:"documents"`
	DocumentsNote     string                  `json:"documents_note,omitempty"`
	Rating            *float64                `json:"rating,omitempty"`
	RatingCount       int                     `json:"rating_count"`
	WeightKG          *float64                `json:"weight_kg"`
	Packages          []shop.Package          `json:"packages"`
	SpecialOrder      bool                    `json:"special_order,omitempty"`
	Availability      nearbyPrice             `json:"availability"`
	Coverage          Coverage                `json:"coverage"`
	Note              string                  `json:"note,omitempty"`
}

func registerGetProduct(s *mcp.Server, d *Deps) {
	addTool(s, &mcp.Tool{
		Name: "get_product",
		Description: "Details of one product for comparing options: description, key selling points, specs, rating, " +
			"package dimensions (mm) and weight, downloadable documents (installation guides, specifications, safety data sheets, " +
			"manuals: PDF links), and the price at the nearest store that sells it in person. " +
			"For plumbing and other fittings, connection_details gathers everything Bunnings states about thread type, gender, " +
			"size and push-fit system, quoted verbatim with its source; Bunnings gives sizes without units or thread standard, " +
			"so do not add one it does not state, and say when thread type or gender is not published. " +
			"Push-fit systems (SmarteX, SharkBite, ...) do not mix: check the brand and compatible pipe. " +
			"weight_kg is null and packages empty when Bunnings does not publish them: say unknown, do not estimate. " +
			"Reviews are a separate tool, get_reviews. " +
			"If availability.sold_nearby is false, do not suggest the product to the user.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in getProductIn) (*mcp.CallToolResult, getProductOut, error) {
		item := strings.TrimSpace(in.ItemNumber)
		if !bunnings.ValidItemNumber(item) {
			return nil, getProductOut{}, fmt.Errorf("item_number must be the digits from search_products, e.g. 0563516")
		}
		a, err := d.nearby(ctx, req, in.Location, in.RadiusKM)
		if err != nil {
			return nil, getProductOut{}, err
		}
		var res *bunnings.SearchResult
		if len(a.stores) > 0 {
			stores := a.searchStores()
			res, err = d.API.Search(ctx, bunnings.SearchRequest{
				AQ: shop.ItemsQuery([]string{item}), Fields: shop.SearchFields(stores, a.region),
				Store: stores[0].ID, Region: a.region, Count: 1,
			})
			if err != nil {
				return nil, getProductOut{}, err
			}
			if len(res.Results) == 0 {
				return nil, getProductOut{}, fmt.Errorf("no Bunnings product %s in the Australian range; check the item number with search_products", item)
			}
		}
		store := ""
		if len(a.stores) > 0 {
			store = a.stores[0].ID
		}
		p, err := d.product(ctx, item, store)
		if err != nil {
			return nil, getProductOut{}, err
		}
		out := getProductOut{
			ItemNumber: item, Name: strings.TrimSpace(p.Name), Brand: p.Brand.Name,
			Description: strings.TrimSpace(p.Feature.Description), KeyPoints: p.Feature.Pointers,
			RatingCount: p.Reviews, WeightKG: shop.ParseWeightKG(p.Weight),
			Packages: packagesOf(p), SpecialOrder: p.IsSpecial, Coverage: a.cov,
		}
		if out.Packages == nil {
			out.Packages = []shop.Package{}
		}
		if p.Reviews > 0 {
			r := round2(p.Rating)
			out.Rating = &r
		}
		out.Specs = shop.Specs(p)
		out.ConnectionDetails = shop.ConnectionDetails(out.Name, out.KeyPoints, out.Description, out.Specs)
		if len(out.ConnectionDetails) == 0 {
			out.ConnectionNote = "Bunnings publishes nothing about how this connects (thread, gender, size or fitting system)."
		} else {
			out.ConnectionNote = "Quoted as published. Sizes carry no unit or thread standard unless the text states one; " +
				"anything not stated here (e.g. thread type or gender) is not published."
		}
		out.Documents = shop.Documents(p)
		if len(out.Documents) == 0 {
			out.DocumentsNote = "Bunnings publishes no documents for this product."
		}

		out.Availability = nearbyPrice{Reason: "not_sold_nearby"}
		if res != nil {
			stores := a.searchStores()
			out.Coverage.StoresCovered = len(stores)
			out.Availability = availability(res, item, stores, &out)
		} else {
			out.Note = a.cov.Note
		}

		summary := fmt.Sprintf("%s %s.", item, out.Name)
		if av := out.Availability; av.SoldNearby {
			summary += fmt.Sprintf(" $%.2f at %s (%.1f km); sold at %s within %.0f km of %s.",
				av.Nearest.Price, av.Nearest.Name, av.Nearest.DistanceKM, plural(av.StoresSelling, "store"), a.radius, a.place.Label())
		} else {
			summary += fmt.Sprintf(" Not available in person within %.0f km of %s (%s). Do not suggest it.", a.radius, a.place.Label(), av.Reason)
		}
		if out.SpecialOrder {
			summary += " Special order: ordered in store, not taken off the shelf."
		}
		if out.WeightKG == nil {
			summary += " Weight not published."
		}
		if len(out.Documents) == 0 {
			summary += " No documents published."
		} else {
			var ds []string
			for _, d := range out.Documents {
				ds = append(ds, d.Type)
			}
			summary += " Documents: " + strings.Join(ds, ", ") + "."
		}
		if len(out.ConnectionDetails) == 0 {
			summary += " No connection details published."
		}
		return textAndJSON(summary, out), out, nil
	})
}

func availability(res *bunnings.SearchResult, item string, stores []shop.Store, out *getProductOut) nearbyPrice {
	var r bunnings.Raw
	for _, x := range res.Results {
		if x.Str("itemnumber") == item {
			r = x
		}
	}
	if r == nil {
		return nearbyPrice{Reason: "not_found"}
	}
	out.URL = shop.URL(r)
	if ex := shop.Exclusion(r); ex != "" {
		return nearbyPrice{Reason: ex}
	}
	offers := shop.Offers(r, stores)
	if len(offers) == 0 {
		return nearbyPrice{Reason: "not_sold_nearby"}
	}
	h := hit(r, offers)
	return nearbyPrice{
		SoldNearby: true, StoresSelling: len(offers), Nearest: &h.NearestStores[0],
		UnitPrice: h.UnitPrice, PriceMin: h.PriceMin, PriceMax: h.PriceMax,
	}
}
