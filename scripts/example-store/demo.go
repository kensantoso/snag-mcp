package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// demoItem is a made-up product on the made-up website.
type demoItem struct {
	number, name string
	price        float64
	stock        int    // in store; -1 for a special order
	aisle, bay   string // where the store keeps it
	weight       string // kg as the product API sends it; "" when it does not
	length       string // longest package side, as the product API sends it
}

// demoList is the README's job: patch a fist-sized hole in a plasterboard
// ceiling and paint it. Prices, stock and places are invented.
var demoList = []demoItem{
	{"9400001", "Patchwell 15cm Plasterboard Repair Patch", 9.98, 14, "21", "6", "0.1", "200 mm"},
	{"9400002", "Smoothset 2.25kg Topping Compound", 14.50, 22, "21", "2", "2.3", "180 mm"},
	{"9400003", "Example 150mm Joint Knife", 8.95, 9, "22", "3", "0.2", "300 mm"},
	{"9400004", "Example Sanding Block, Medium Grit", 4.20, 31, "22", "7", "", "120 mm"},
	{"9400005", "Example Paints 4L Ceiling White", 59.00, 12, "5", "4", "5.4", "200 mm"},
	{"9400006", "Example 270mm Roller Kit", 19.90, 7, "6", "12", "0.6", "330 mm"},
	{"9400007", "Example 1.2-2.4m Extension Pole", 24.95, 5, "6", "15", "0.9", "1.25 mtr"},
	{"9400008", "Example Plastic Drop Sheet 3.6 x 2.7m", 6.48, 40, "4", "8", "", "250 mm"},
	{"9400009", "Example 3 Step Folding Ladder", 69.00, 4, "15", "3", "6.2", "1.6 mtr"},
	{"9400010", "Example 600 x 600mm Ceiling Access Panel", 32.00, -1, "", "", "1.8", "620 mm"},
}

// runDemo serves the store and the list from a fake website, connects the
// real tools to it, and calls check_list as a host that shows MCP Apps.
func runDemo(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	site := httptest.NewServer(demoSite(files))
	defer site.Close()
	api := bunnings.New(4)
	api.AuthURL, api.BaseURL = site.URL+"/connect/authorize", site.URL+"/_apis"
	api.MapBaseURL, api.MapCacheDir = site.URL, filepath.Join(dir, "cache")
	defer os.RemoveAll(api.MapCacheDir)

	srv := mcp.NewServer(&mcp.Implementation{Name: "snag", Version: "demo"}, nil)
	tools.Register(srv, &tools.Deps{API: api, ConfigPath: filepath.Join(dir, "config.json")})
	defer os.Remove(filepath.Join(dir, "config.json"))
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return err
	}
	caps := &mcp.ClientCapabilities{}
	caps.AddExtension("io.modelcontextprotocol/ui", map[string]any{"mimeTypes": []string{"text/html;profile=mcp-app"}})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "example-store", Version: "1"}, &mcp.ClientOptions{Capabilities: caps}).
		Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	var check *mcp.Tool
	for _, t := range list.Tools {
		if t.Name == "check_list" {
			check = t
		}
	}
	view, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ui://snag/pick-list"})
	if err != nil {
		return err
	}
	var items []map[string]any
	for _, it := range demoList {
		items = append(items, map[string]any{"item_number": it.number, "quantity": 1})
	}
	args := map[string]any{"store": "0001", "location": "3000", "items": items}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "check_list", Arguments: args})
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("check_list: %v", res.Content)
	}
	var out struct {
		PickList struct {
			Store struct {
				ID string `json:"store_id"`
			} `json:"store"`
		} `json:"pick_list"`
		StoreMap struct {
			MapID string `json:"map_id"`
			Stops []struct {
				Address string `json:"address"`
			} `json:"stops"`
		} `json:"store_map"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	var stops []string
	for _, s := range out.StoreMap.Stops {
		stops = append(stops, s.Address)
	}
	// In the order the view sends them, which preview.py checks.
	largs := struct {
		StoreID string   `json:"store_id"`
		MapID   string   `json:"map_id"`
		Stops   []string `json:"stops"`
	}{out.PickList.Store.ID, out.StoreMap.MapID, stops}
	layout, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_store_layout", Arguments: largs})
	if err != nil {
		return err
	}

	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.ImageContent:
			if err := os.WriteFile(filepath.Join(dir, "map-example.png"), c.Data, 0o644); err != nil {
				return err
			}
		case *mcp.TextContent:
			if !strings.HasPrefix(c.Text, "{") {
				if err := os.WriteFile(filepath.Join(dir, "pick-list.txt"), []byte(c.Text+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
	}
	// What preview.py records from a live server, so it can replay this.
	rec, err := json.Marshal(map[string]any{
		"tool": check, "view": view.Contents[0], "arguments": args, "result": res,
		"layout": map[string]any{"arguments": largs, "result": layout},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "example.json"), rec, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", filepath.Join(dir, "pick-list.txt"), filepath.Join(dir, "map-example.png"), filepath.Join(dir, "example.json"))
	return nil
}

// demoSite is a website that answers the requests check_list makes, for one
// store near Melbourne's CBD, with demoList as its range.
func demoSite(files map[string][]byte) http.Handler {
	byNumber := map[string]demoItem{}
	for _, it := range demoList {
		byNumber[it.number] = it
	}
	ok := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"statusDetails": map[string]any{"state": "SUCCESS"}, "data": data})
	}
	item := regexp.MustCompile(`^/_apis/v[12]/products/([0-9]+)(/fulfillment)?$`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/connect/authorize":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "demo", Path: "/"})
			http.Redirect(w, r, "/callback", http.StatusFound)
		case p == "/callback":
			http.Redirect(w, r, "https://www.bunnings.com.au/static/guest.html#access_token=demo&expires_in=432000", http.StatusFound)
		case p == "/_apis/v1/stores":
			ok(w, map[string]any{"pagination": map[string]any{"totalPages": 1}, "stores": []any{map[string]any{
				"name": store, "displayName": name, "pricingRegion": "VICMetro", "isActiveLocation": true,
				"geoPoint": map[string]any{"latitude": -37.8155, "longitude": 144.9750},
				"address":  map[string]any{"formattedAddress": "1 Example Street, Melbourne VIC 3000", "region": map[string]any{"isocode": "VIC"}},
			}}})
		case p == "/_apis/v1/coveo/search":
			var body struct {
				AQ string `json:"aq"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			var results []any
			for _, n := range regexp.MustCompile(`[0-9]{7}`).FindAllString(body.AQ, -1) {
				it, found := byNumber[n]
				if !found {
					continue
				}
				ful := []string{"In-Store", "Click & Collect", "Delivery"}
				if it.stock < 0 {
					ful = []string{"In-Store", "Delivery"}
				}
				results = append(results, map[string]any{"raw": map[string]any{
					"itemnumber": it.number, "name": it.name, "brandname": strings.Fields(it.name)[0], "isactive": "true",
					"price_" + store: it.price, "fulfilmentoptions_" + store: ful,
				}})
			}
			ok(w, map[string]any{"totalCount": len(results), "results": results})
		case item.MatchString(p):
			m := item.FindStringSubmatch(p)
			it, found := byNumber[m[1]]
			if !found {
				http.NotFound(w, r)
				return
			}
			if m[2] != "" {
				var stock *int
				if it.stock >= 0 {
					stock = &it.stock
				}
				ok(w, map[string]any{"isActive": true, "isSpecialProduct": it.stock < 0,
					"inStorePickUpData": map[string]any{"stock": stock, "storeName": name},
					"clickNCollectData": map[string]any{"stock": stock, "isClicknCollectAvailable": it.stock > 0}})
				return
			}
			prod := map[string]any{"itemNumber": it.number, "name": it.name, "isActive": true, "isSpecialProduct": it.stock < 0,
				"dimension": map[string]any{"packages": []any{map[string]any{"length": it.length, "width": "100 mm", "height": "50 mm"}}}}
			if it.weight != "" {
				prod["weight"] = it.weight
			}
			ok(w, prod)
		case p == "/_apis/v1/item-api/locations":
			it := byNumber[r.URL.Query().Get("productCode")]
			var locs []any
			if it.aisle != "" {
				locs = append(locs, map[string]any{"aisle": it.aisle, "bay": it.bay})
			}
			ok(w, []any{map[string]any{"inStoreLocations": locs}})
		case p == "/api/stores-manifest.json":
			fmt.Fprintf(w, `{"stores":{%q:{"mapId":%q}}}`, store, mapID)
		case strings.HasPrefix(p, "/api/"+store+"/"+mapID+"-"):
			kind := strings.TrimSuffix(strings.TrimPrefix(p, "/api/"+store+"/"+mapID+"-"), ".json")
			if b, found := files[kind]; found {
				w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
