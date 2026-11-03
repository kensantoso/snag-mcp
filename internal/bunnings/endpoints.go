package bunnings

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

var itemNumberRE = regexp.MustCompile(`^[0-9]{4,8}$`)

// ValidItemNumber reports whether s looks like a Bunnings item number. Item
// numbers go into URL paths, so anything else is refused before it is sent.
func ValidItemNumber(s string) bool { return itemNumberRE.MatchString(s) }

// Store is a Bunnings store as /v1/stores returns it.
type Store struct {
	ID            string // the gateway's "name", e.g. "6400"
	Name          string // displayName, e.g. "Mentone"
	State         string
	Address       string
	Lat, Lon      float64
	PricingRegion string // e.g. "VICMetro"; search needs it as X-region
	MapURL        string
}

type wireStore struct {
	Name          string `json:"name"`
	DisplayName   string `json:"displayName"`
	PricingRegion string `json:"pricingRegion"`
	MapURL        string `json:"mapUrl"`
	IsActive      *bool  `json:"isActiveLocation"`
	GeoPoint      struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"geoPoint"`
	Address struct {
		FormattedAddress string `json:"formattedAddress"`
		State            string `json:"state"`
		Region           struct {
			Isocode string `json:"isocode"`
		} `json:"region"`
	} `json:"address"`
}

// StoresNear returns active stores around a point, nearest first, as far out as
// radiusKM. The gateway's radius is approximate, so callers that care about the
// boundary should measure distance themselves.
//
// A point with no store in range is an empty slice, not an error.
func (c *Client) StoresNear(ctx context.Context, lat, lon, radiusKM float64, max int) ([]Store, error) {
	const pageSize = 100
	var out []Store
	for page := 0; ; page++ {
		q := url.Values{
			"latitude":    {strconv.FormatFloat(lat, 'f', 5, 64)},
			"longitude":   {strconv.FormatFloat(lon, 'f', 5, 64)},
			"currentPage": {strconv.Itoa(page)},
			"fields":      {"FULL"},
			"pageSize":    {strconv.Itoa(min(pageSize, max))},
			"radius":      {strconv.Itoa(int(radiusKM * 1000))},
		}
		var data struct {
			Pagination struct {
				TotalPages int `json:"totalPages"`
			} `json:"pagination"`
			Stores []wireStore `json:"stores"`
		}
		err := c.call(ctx, http.MethodGet, "/v1/stores?"+q.Encode(), "", "", nil, &data)
		if IsCode(err, "BUS-4455") { // "No stores found within the given search radius"
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, w := range data.Stores {
			if w.IsActive != nil && !*w.IsActive {
				continue
			}
			st := w.Address.Region.Isocode
			if st == "" {
				st = w.Address.State
			}
			out = append(out, Store{
				ID: w.Name, Name: w.DisplayName, State: st,
				Address: w.Address.FormattedAddress,
				Lat:     w.GeoPoint.Latitude, Lon: w.GeoPoint.Longitude,
				PricingRegion: w.PricingRegion, MapURL: w.MapURL,
			})
		}
		if len(out) >= max || page+1 >= data.Pagination.TotalPages || len(data.Stores) == 0 {
			if len(out) > max {
				out = out[:max]
			}
			return out, nil
		}
	}
}

// SearchRequest is a product search. AQ is a Coveo advanced query and Fields
// the raw fields to return; per-store fields such as price_6400 can be listed
// for many stores in one request.
type SearchRequest struct {
	Query  string
	AQ     string
	Fields []string
	Store  string // context store, normally the nearest
	Region string // that store's pricing region
	First  int
	Count  int
	// Facet, when set (e.g. "@brandname"), asks for that field's values and
	// counts across every match, up to FacetMax of them.
	Facet    string
	FacetMax int
}

// SearchResult holds the raw fields of each hit.
type SearchResult struct {
	Total   int
	Results []Raw
	Facet   []FacetValue // the requested facet, most common first
}

// FacetValue is one value of a facet and how many matches carry it.
type FacetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Search runs a product search through the gateway's Coveo proxy, with the
// body the website itself sends, trimmed to what matters.
func (c *Client) Search(ctx context.Context, r SearchRequest) (*SearchResult, error) {
	body := map[string]any{
		"q":                     r.Query,
		"firstResult":           r.First,
		"numberOfResults":       r.Count,
		"sortCriteria":          "relevancy",
		"searchHub":             "PRODUCT_SEARCH",
		"pipeline":              "Variant_Product",
		"cq":                    "@source==(PRODUCT_STREAM_AU)",
		"aq":                    r.AQ,
		"context":               map[string]string{"store": r.Store, "country": "Australia", "platform": "Web", "role": "retail", "website": "AU"},
		"context_website":       "AU",
		"cu":                    "AUD",
		"de":                    "UTF-8",
		"enableDidYouMean":      true,
		"partialMatch":          true,
		"partialMatchKeywords":  2,
		"partialMatchThreshold": "30%",
		"enableWordCompletion":  true,
		"lowerCaseOperators":    true,
		"questionMark":          true,
		"isGuestUser":           true,
		"fieldsToInclude":       r.Fields,
	}
	if r.Facet != "" {
		body["groupBy"] = []map[string]any{{
			"field": r.Facet, "maximumNumberOfValues": max(r.FacetMax, 1),
			"sortCriteria": "occurrences", "injectionDepth": 1000000,
		}}
	}
	var data struct {
		TotalCount int `json:"totalCount"`
		Results    []struct {
			Raw Raw `json:"raw"`
		} `json:"results"`
		GroupBy []struct {
			Values []struct {
				Value string `json:"value"`
				Count int    `json:"numberOfResults"`
			} `json:"values"`
		} `json:"groupByResults"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/coveo/search", r.Store, r.Region, body, &data); err != nil {
		return nil, err
	}
	out := &SearchResult{Total: data.TotalCount}
	for _, x := range data.Results {
		out.Results = append(out.Results, x.Raw)
	}
	for _, g := range data.GroupBy {
		for _, v := range g.Values {
			out.Facet = append(out.Facet, FacetValue{v.Value, v.Count})
		}
	}
	return out, nil
}

// Fulfilment is an item's availability at one store.
type Fulfilment struct {
	IsActive      bool
	IsMarketLink  bool
	IsSpecial     bool // special order: ordered in store, no shelf stock
	InStoreStock  *int // nil when the gateway gives no count
	InStoreMsg    string
	ClickNCollect bool
	StoreName     string
	StoreMapURL   string
}

// Fulfilment returns live stock for an item at a store. Stock is an exact count.
func (c *Client) Fulfilment(ctx context.Context, item, store string) (*Fulfilment, error) {
	if !ValidItemNumber(item) {
		return nil, fmt.Errorf("invalid item number %q", item)
	}
	body := map[string]any{"includeVariantStock": true, "locationCode": store, "storeRadius": 20}
	var data struct {
		IsActive         bool `json:"isActive"`
		IsMarketLink     bool `json:"isMarketLink"`
		IsSpecialProduct bool `json:"isSpecialProduct"`
		InStore          *struct {
			Stock       *int   `json:"stock"`
			Msg         string `json:"inStoreStockMsg"`
			StoreName   string `json:"storeName"`
			StoreMapURL string `json:"storeMapUrl"`
		} `json:"inStorePickUpData"`
		CNC *struct {
			Stock     *int `json:"stock"`
			Available bool `json:"isClicknCollectAvailable"`
		} `json:"clickNCollectData"`
	}
	if err := c.call(ctx, http.MethodPost, "/v2/products/"+item+"/fulfillment", store, "", body, &data); err != nil {
		return nil, err
	}
	f := &Fulfilment{IsActive: data.IsActive, IsMarketLink: data.IsMarketLink, IsSpecial: data.IsSpecialProduct}
	if data.InStore != nil {
		f.InStoreStock, f.InStoreMsg = data.InStore.Stock, data.InStore.Msg
		f.StoreName, f.StoreMapURL = data.InStore.StoreName, data.InStore.StoreMapURL
	}
	if data.CNC != nil {
		f.ClickNCollect = data.CNC.Available
		if f.InStoreStock == nil {
			f.InStoreStock = data.CNC.Stock
		}
	}
	return f, nil
}

// StoreStock is one product's stock at one store from the multi-store lookup.
type StoreStock struct {
	// Level is an exact count, nil when the store has none to report. A
	// special-order item reads either nil or the placeholder 9999 here, so a
	// caller must know special orders another way; see Product.
	Level *int
	// Visible is false when the store does not range the item at all.
	Visible bool
}

// placeholderStock is what the multi-store lookup reports for some special
// orders in place of a count. It is never a real shelf count.
const placeholderStock = 9999

// StockNear returns live stock for up to 30 items at every store around a
// point, keyed by store id then item number. This is what the website's "find
// a store" panel calls: one request covers twenty stores, so a whole list
// across a city is a few calls rather than one per item per store.
func (c *Client) StockNear(ctx context.Context, lat, lon, radiusKM float64, items []string) (map[string]map[string]StoreStock, error) {
	for _, it := range items {
		if !ValidItemNumber(it) {
			return nil, fmt.Errorf("invalid item number %q", it)
		}
	}
	const pageSize = 20 // the gateway's maximum
	const maxPages = 10
	out := map[string]map[string]StoreStock{}
	for page := 0; page < maxPages; page++ {
		q := url.Values{
			"fields":      {"BASIC"},
			"isPickup":    {"true"},
			"isTrCart":    {"false"},
			"latitude":    {strconv.FormatFloat(lat, 'f', 5, 64)},
			"longitude":   {strconv.FormatFloat(lon, 'f', 5, 64)},
			"pageSize":    {strconv.Itoa(pageSize)},
			"radius":      {strconv.Itoa(int(radiusKM * 1000))},
			"currentPage": {strconv.Itoa(page)},
		}
		var data struct {
			Pagination struct {
				TotalPages int `json:"totalPages"`
			} `json:"pagination"`
			Stores []struct {
				Name     string `json:"name"`
				Products []struct {
					Code  string `json:"code"`
					Stock struct {
						Level *int `json:"stockLevel"`
					} `json:"stock"`
					Visible bool `json:"visible"`
				} `json:"products"`
			} `json:"stores"`
		}
		err := c.call(ctx, http.MethodPost, "/v1/stores/products/stock?"+q.Encode(), "", "", map[string]any{"products": items}, &data)
		if IsCode(err, "BUS-4455") {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, s := range data.Stores {
			m := map[string]StoreStock{}
			for _, p := range s.Products {
				lv := p.Stock.Level
				if lv != nil && *lv >= placeholderStock {
					lv = nil
				}
				m[p.Code] = StoreStock{Level: lv, Visible: p.Visible}
			}
			out[s.Name] = m
		}
		if page+1 >= data.Pagination.TotalPages || len(data.Stores) == 0 {
			break
		}
	}
	return out, nil
}

// Location is where an item sits in a store.
type Location struct {
	Aisle, Bay string
}

// Locations returns the aisle and bay of an item at a store, in the store's
// own order. Empty when the store has no location on file.
func (c *Client) Locations(ctx context.Context, item, store string) ([]Location, error) {
	if !ValidItemNumber(item) {
		return nil, fmt.Errorf("invalid item number %q", item)
	}
	q := url.Values{"locationCode": {store}, "productCode": {item}}
	var data []struct {
		InStoreLocations []struct {
			Aisle string `json:"aisle"`
			Bay   string `json:"bay"`
		} `json:"inStoreLocations"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/item-api/locations?"+q.Encode(), store, "", nil, &data); err != nil {
		return nil, err
	}
	var out []Location
	for _, d := range data {
		for _, l := range d.InStoreLocations {
			out = append(out, Location{Aisle: l.Aisle, Bay: l.Bay})
		}
	}
	return out, nil
}

// Product is the subset of /v1/products/{item} worth comparing on.
type Product struct {
	ItemNumber   string  `json:"itemNumber"`
	Name         string  `json:"name"`
	IsActive     bool    `json:"isActive"`
	IsMarketLink bool    `json:"isMarketLink"`
	IsSpecial    bool    `json:"isSpecialProduct"`
	Rating       float64 `json:"averageRating"`
	Reviews      int     `json:"numberOfReviews"`
	Brand        struct {
		Name string `json:"name"`
	} `json:"brand"`
	Feature struct {
		Description string   `json:"description"`
		Pointers    []string `json:"pointers"`
	} `json:"feature"`
	Classifications []struct {
		Features []struct {
			Name   string `json:"name"`
			Values []struct {
				Value string `json:"value"`
			} `json:"featureValues"`
		} `json:"features"`
	} `json:"classifications"`
	// GuideDocument lists the PDFs on the product page's downloads: installation
	// guides, specifications, safety data sheets, manuals. Often absent.
	GuideDocument []struct {
		DocType string `json:"docType"`
		URL     string `json:"url"`
		Mime    string `json:"mime"`
		Size    int64  `json:"size"`
	} `json:"guideDocument"`
	// Weight is in kilograms, sent as a string, and often absent.
	Weight    json.RawMessage `json:"weight"`
	Dimension struct {
		// Packages are what the item ships as. dimension.product exists too
		// and is wrong often enough (6000 x 70 for a 3.0 m 90x45) that it is
		// deliberately not decoded.
		Packages []struct {
			Length string `json:"length"`
			Width  string `json:"width"`
			Height string `json:"height"`
		} `json:"packages"`
	} `json:"dimension"`
}

// Product returns product details as seen from a store. The store matters:
// whether an item is a special order is answered for the store in the
// locationCode header, and without one it reads false.
func (c *Client) Product(ctx context.Context, item, store string) (*Product, error) {
	if !ValidItemNumber(item) {
		return nil, fmt.Errorf("invalid item number %q", item)
	}
	var p Product
	if err := c.call(ctx, http.MethodGet, "/v1/products/"+item, store, "", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Raw is one search hit's raw fields. Values arrive loosely typed (isactive is
// the string "true"), so they are read through accessors.
type Raw map[string]json.RawMessage

// Str returns a string field, or "" when absent or not a string.
func (r Raw) Str(k string) string {
	var s string
	if json.Unmarshal(r[k], &s) != nil {
		return ""
	}
	return s
}

// Num returns a numeric field, accepting numbers sent as strings.
func (r Raw) Num(k string) (float64, bool) {
	v, ok := r[k]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(v, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Strs returns a multi-value field. A single string is returned as one value.
func (r Raw) Strs(k string) []string {
	v, ok := r[k]
	if !ok {
		return nil
	}
	var ss []string
	if json.Unmarshal(v, &ss) == nil {
		return ss
	}
	if s := r.Str(k); s != "" {
		return []string{s}
	}
	return nil
}
