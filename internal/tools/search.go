package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/geo"
	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- set_location ----------

type setLocationIn struct {
	PostcodeOrSuburb string `json:"postcode_or_suburb" jsonschema:"The user's Australian postcode or suburb as they gave it, e.g. '3194', 'Mentone' or 'Richmond VIC'. Add the state when a suburb name exists in several states."`
}

type setLocationOut struct {
	Location        string       `json:"location"`
	Postcode        string       `json:"postcode"`
	State           string       `json:"state"`
	StoresWithin50  int          `json:"stores_within_50_km"`
	NearestStores   []shop.Store `json:"nearest_stores"`
	SavedTo         string       `json:"saved_to"`
	Attribution     string       `json:"attribution"`
	NoStoreNearNote string       `json:"note,omitempty"`
}

func registerSetLocation(s *mcp.Server, d *Deps) {
	addTool(s, &mcp.Tool{
		Name: "set_location",
		Description: "Save the user's location (an Australian postcode or suburb) so every other tool finds stores near it. " +
			"Call this when the user tells you where they are, or when another tool reports the location is not set. " +
			"Ask the user; never guess a location. It is remembered across sessions, and any tool's `location` argument " +
			"overrides it for one call without changing it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in setLocationIn) (*mcp.CallToolResult, setLocationOut, error) {
		p, err := geo.Resolve(in.PostcodeOrSuburb)
		if err != nil {
			return nil, setLocationOut{}, err
		}
		if err := d.SaveLocation(p); err != nil {
			return nil, setLocationOut{}, fmt.Errorf("could not save location: %w", err)
		}
		a, err := d.around(ctx, p, defaultRadiusKM)
		if err != nil {
			return nil, setLocationOut{}, err
		}
		out := setLocationOut{
			Location: p.Label(), Postcode: p.Postcode, State: p.State,
			StoresWithin50: len(a.stores), SavedTo: d.ConfigPath, Attribution: geo.Attribution,
			NearestStores: a.stores[:min(5, len(a.stores))],
		}
		summary := fmt.Sprintf("Location saved: %s. %s within 50 km", p.Label(), plural(len(a.stores), "Bunnings store"))
		if len(a.stores) > 0 {
			summary += fmt.Sprintf("; nearest is %s (%.1f km).", a.stores[0].Name, a.stores[0].DistanceKM)
		} else {
			out.NoStoreNearNote = a.cov.Note
			summary += ". " + a.cov.Note
		}
		return textAndJSON(summary, out), out, nil
	})
}

// ---------- search_products ----------

type searchIn struct {
	Query    string  `json:"query" jsonschema:"What to look for, in shop words: 'masonry drill bit 10mm', '90x45 treated pine', 'decking screws'"`
	Location string  `json:"location,omitempty" jsonschema:"Postcode or suburb for this search only. Omit to use the saved location."`
	RadiusKM float64 `json:"radius_km,omitempty" jsonschema:"How far to look for stores, in km. Default 50. Widen only after asking the user."`
	Limit    int     `json:"limit,omitempty" jsonschema:"Maximum products to return. Default 10, at most 25."`
	Brand    string  `json:"brand,omitempty" jsonschema:"Only this brand, as the user says it: 'smartex', 'SharkBite', 'full boar'. Use it whenever the user names a brand or a fitting system (push-fit systems do not mix). Matched to Bunnings' spelling; the brand need not be repeated in query."`
}

type storePrice struct {
	StoreID    string  `json:"store_id"`
	Name       string  `json:"name"`
	DistanceKM float64 `json:"distance_km"`
	Price      float64 `json:"price"`
}

type unitPrice struct {
	Price float64 `json:"price"`
	Per   string  `json:"per,omitempty"`
}

type promotion struct {
	Campaign string `json:"campaign"`
	Ends     string `json:"ends,omitempty"`
}

type productHit struct {
	ItemNumber    string       `json:"item_number"`
	Name          string       `json:"name"`
	Brand         string       `json:"brand,omitempty"`
	PriceMin      float64      `json:"price_min"`
	PriceMax      float64      `json:"price_max"`
	StoresSelling int          `json:"stores_selling"`
	NearestStores []storePrice `json:"nearest_stores"`
	Rating        *float64     `json:"rating,omitempty"`
	RatingCount   int          `json:"rating_count,omitempty"`
	UnitPrice     *unitPrice   `json:"unit_price,omitempty"`
	Promotion     *promotion   `json:"promotion,omitempty"`
	URL           string       `json:"url"`
}

type searchOut struct {
	Query    string       `json:"query"`
	Products []productHit `json:"products"`
	// Matches is how many products Bunnings matched before the nearby filter;
	// ExcludedNotNearby how many of the ones examined were dropped by it.
	Matches           int      `json:"bunnings_matches"`
	ExcludedNotNearby int      `json:"excluded_not_sold_nearby"`
	Coverage          Coverage `json:"coverage"`
	// Brand filter: the brands the user's spelling matched, and when it
	// matched none, near spellings and the brands that do have matches.
	BrandsMatched    []string              `json:"brands_matched,omitempty"`
	BrandSuggestions []string              `json:"brand_suggestions,omitempty"`
	BrandsForQuery   []bunnings.FacetValue `json:"brands_for_query,omitempty"`
	// MatchedAnyWord is set when no product had every word of the query and
	// the results match only some of them.
	MatchedAnyWord bool   `json:"matched_any_word,omitempty"`
	Note           string `json:"note,omitempty"`
}

func registerSearchProducts(s *mcp.Server, d *Deps) {
	addTool(s, &mcp.Tool{
		Name: "search_products",
		Description: "Search Bunnings for products the user can actually buy in person near them. " +
			"Only items sold In-Store or Click & Collect at a store within radius_km of the user's location are returned; " +
			"marketplace and delivery-only listings never are. Each result has the item_number the other tools need, " +
			"the price range across nearby stores that sell it, how many sell it and the nearest few with prices. " +
			"Use it before suggesting any product, and run several narrower searches rather than one broad one. " +
			"Pass `brand` when the user names a brand or system; if it matches nothing, the result lists near spellings and the " +
			"brands that do match the query, so you can correct it or tell the user. " +
			"Prices differ by store and region, so quote them as 'from $x near you'.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, searchOut{}, fmt.Errorf("query is required")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		limit = min(limit, 25)
		a, err := d.nearby(ctx, req, in.Location, in.RadiusKM)
		if err != nil {
			return nil, searchOut{}, err
		}
		out := searchOut{Query: in.Query, Products: []productHit{}, Coverage: a.cov}
		if len(a.stores) == 0 {
			return textAndJSON(a.cov.Note, out), out, nil
		}
		stores := a.searchStores()
		out.Coverage.StoresCovered = len(stores)
		query, aq := in.Query, shop.AvailabilityQuery(stores)
		search := func(q string) (*bunnings.SearchResult, error) {
			return d.API.Search(ctx, bunnings.SearchRequest{
				Query: q, AQ: aq, Fields: shop.SearchFields(stores, a.region),
				Store: stores[0].ID, Region: a.region,
				// Over-fetch: the server-side filter is a first cut, and
				// anything failing the full rule here is dropped.
				Count: min(limit*2+5, 50),
			})
		}
		// brandsFor reports which brands do match the query nearby, so a
		// wrong or empty brand can be corrected rather than guessed at.
		brandsFor := func() {
			if r, err := d.API.Search(ctx, bunnings.SearchRequest{
				Query: in.Query, AQ: shop.AvailabilityQuery(stores), Store: stores[0].ID, Region: a.region,
				Count: 0, Facet: "@brandname", FacetMax: 15,
			}); err == nil {
				out.BrandsForQuery = r.Facet
			}
		}

		if strings.TrimSpace(in.Brand) != "" {
			all, err := d.brandsNear(ctx, stores, a.region)
			if err != nil {
				return nil, searchOut{}, err
			}
			matched, suggest := shop.MatchBrands(in.Brand, all)
			if len(matched) == 0 {
				out.BrandSuggestions = suggest
				brandsFor()
				out.Note = fmt.Sprintf("No brand like %q is sold in person within %.0f km. See brand_suggestions for near spellings "+
					"and brands_for_query for the brands that match the query; retry with one, or ask the user.", in.Brand, a.radius)
				return textAndJSON(out.Note+brandLine(out), out), out, nil
			}
			out.BrandsMatched = matched
			aq += " AND " + shop.BrandQuery(matched)
			query = shop.StripBrand(query, append([]string{in.Brand}, matched...)...)
		}

		res, err := search(query)
		if err != nil {
			return nil, searchOut{}, err
		}
		qualify := func(res *bunnings.SearchResult) {
			out.Matches = res.Total
			out.ExcludedNotNearby = 0
			for _, r := range res.Results {
				offers := shop.Offers(r, stores)
				if len(offers) == 0 {
					out.ExcludedNotNearby++
					continue
				}
				if len(out.Products) < limit {
					out.Products = append(out.Products, hit(r, offers))
				}
			}
		}
		qualify(res)
		// Within one brand, every word of a query often matches nothing
		// because the brand names things its own way ("16 x 15mm" rather than
		// "16mm"). One retry on any word, flagged as such.
		if len(out.Products) == 0 && len(out.BrandsMatched) > 0 {
			if anyq := shop.AnyWordQuery(query); anyq != "" {
				if res, err := search(anyq); err == nil {
					qualify(res)
					out.MatchedAnyWord = len(out.Products) > 0
				}
			}
		}

		summary := fmt.Sprintf("%s for %q sold in person within %.0f km of %s (%s checked).",
			plural(len(out.Products), "product"), in.Query, a.radius, a.place.Label(), plural(len(stores), "store"))
		if len(out.BrandsMatched) > 0 {
			summary += " Brand: " + strings.Join(out.BrandsMatched, ", ") + "."
		}
		switch {
		case len(out.Products) == 0 && len(out.BrandsMatched) > 0:
			brandsFor()
			out.Note = fmt.Sprintf("Nothing from %s matching the query is sold in a store within the radius. brands_for_query lists "+
				"the brands that do match; do not substitute one without telling the user, as fitting systems do not mix.",
				strings.Join(out.BrandsMatched, ", "))
			summary += " " + out.Note + brandLine(out)
		case len(out.Products) == 0:
			out.Note = "Nothing matching is sold in a store within the radius. Try other words, or ask the user before widening radius_km."
			summary += " " + out.Note
		case out.MatchedAnyWord:
			out.Note = "No product had every word of the query; these match some of the words. Check each one fits the need."
			summary += " " + out.Note
		}
		for _, p := range out.Products {
			summary += fmt.Sprintf("\n- %s %s: %s at %s", p.ItemNumber, p.Name, priceRange(p.PriceMin, p.PriceMax), plural(p.StoresSelling, "store"))
		}
		return textAndJSON(summary, out), out, nil
	})
}

func hit(r bunnings.Raw, offers []shop.Offer) productHit {
	h := productHit{
		ItemNumber: r.Str("itemnumber"), Name: strings.TrimSpace(r.Str("name")), Brand: r.Str("brandname"),
		StoresSelling: len(offers), URL: shop.URL(r), PriceMin: offers[0].Price, PriceMax: offers[0].Price,
	}
	for _, o := range offers {
		h.PriceMin, h.PriceMax = min(h.PriceMin, o.Price), max(h.PriceMax, o.Price)
	}
	for _, o := range offers[:min(3, len(offers))] {
		h.NearestStores = append(h.NearestStores, storePrice{o.Store.ID, o.Store.Name, o.Store.DistanceKM, o.Price})
	}
	if rc, ok := r.Num("ratingcount"); ok && rc > 0 {
		h.RatingCount = int(rc)
		if v, ok := r.Num("rating"); ok {
			v = round2(v)
			h.Rating = &v
		}
	}
	h.UnitPrice = unitPriceOf(r, offers[0])
	if c := r.Str("promotionalcampaign"); c != "" {
		h.Promotion = &promotion{Campaign: c, Ends: r.Str("promotionalcampaignend")}
	}
	return h
}

func unitPriceOf(r bunnings.Raw, o shop.Offer) *unitPrice {
	if o.UnitPrice <= 0 {
		return nil
	}
	return &unitPrice{Price: o.UnitPrice, Per: r.Str("comparisonunitofmeasure")}
}

func priceRange(lo, hi float64) string {
	if lo == hi {
		return fmt.Sprintf("$%.2f", lo)
	}
	return fmt.Sprintf("$%.2f-$%.2f", lo, hi)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func brandLine(out searchOut) string {
	var s string
	if len(out.BrandSuggestions) > 0 {
		s += "\nDid you mean: " + strings.Join(out.BrandSuggestions, ", ")
	}
	if len(out.BrandsForQuery) > 0 {
		var bs []string
		for _, b := range out.BrandsForQuery {
			bs = append(bs, fmt.Sprintf("%s (%d)", b.Value, b.Count))
		}
		s += "\nBrands matching the query nearby: " + strings.Join(bs, ", ")
	}
	return s
}
