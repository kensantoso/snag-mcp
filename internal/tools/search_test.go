package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeBunnings is the guest login, the store list (Northfield, an invented
// store, alone) and Coveo
// search, with search answered by a test function and every query recorded.
type fakeBunnings struct {
	*httptest.Server
	brands []string // the brands-near facet
	search func(q, aq string) []bunnings.Raw

	mu      sync.Mutex
	queries []string // "q | aq" of each product search
}

func newFakeBunnings(t *testing.T) *fakeBunnings {
	f := &fakeBunnings{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect/authorize":
			http.SetCookie(w, &http.Cookie{Name: "idsrv", Value: "x", Path: "/"})
			http.Redirect(w, r, "/callback", http.StatusFound)
			return
		case "/callback":
			http.Redirect(w, r, "https://www.bunnings.com.au/static/guest.html#access_token=T&expires_in=432000", http.StatusFound)
			return
		case "/_apis/v1/stores":
			fmt.Fprint(w, `{"statusDetails":{"state":"SUCCESS"},"data":{"pagination":{"totalPages":1},"stores":[
				{"name":"0101","displayName":"Northfield","pricingRegion":"VICMetro","geoPoint":{"latitude":-37.98,"longitude":145.07},
				 "address":{"region":{"isocode":"VIC"}}}]}}`)
			return
		case "/_apis/v1/coveo/search":
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		var body struct {
			Q       string           `json:"q"`
			AQ      string           `json:"aq"`
			GroupBy []map[string]any `json:"groupBy"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		var data struct {
			TotalCount int              `json:"totalCount"`
			Results    []map[string]any `json:"results"`
			GroupBy    []map[string]any `json:"groupByResults"`
		}
		if len(body.GroupBy) > 0 {
			var vals []map[string]any
			for _, b := range f.brands {
				vals = append(vals, map[string]any{"value": b, "numberOfResults": 1})
			}
			data.GroupBy = []map[string]any{{"values": vals}}
		} else {
			f.mu.Lock()
			f.queries = append(f.queries, body.Q+" | "+body.AQ)
			f.mu.Unlock()
			for _, raw := range f.search(body.Q, body.AQ) {
				data.Results = append(data.Results, map[string]any{"raw": raw})
			}
			data.TotalCount = len(data.Results)
		}
		json.NewEncoder(w).Encode(map[string]any{"statusDetails": map[string]any{"state": "SUCCESS"}, "data": data})
	}))
	t.Cleanup(f.Close)
	return f
}

// session runs the tools against the fake over an in-memory MCP connection.
func (f *fakeBunnings) session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	api := bunnings.New(2)
	api.AuthURL, api.BaseURL = f.URL+"/connect/authorize", f.URL+"/_apis"
	return sessionFor(t, api)
}

// sessionFor runs the tools over api through an in-memory MCP connection.
func sessionFor(t *testing.T, api *bunnings.Client) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	Register(srv, &Deps{API: api, ConfigPath: filepath.Join(t.TempDir(), "config.json")})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func searchTool(t *testing.T, cs *mcp.ClientSession, args map[string]any) searchOut {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_products", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("search_products: %v %+v", err, res)
	}
	var out searchOut
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	return out
}

// referenceRaws are the invented search hits from the shop package's tests.
func referenceRaws(t *testing.T) map[string]bunnings.Raw {
	t.Helper()
	b, err := os.ReadFile("../shop/testdata/reference_items.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []bunnings.Raw
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]bunnings.Raw{}
	for _, r := range list {
		out[r.Str("itemnumber")] = r
	}
	return out
}

// A brand filter narrows a search; it never widens what counts as available.
// Even if the brand matched and Coveo returned the trap items, the WA-only
// cement (a stale $1.54 at Northfield, no in-store fulfilment) and the
// marketplace shower seat are dropped, through the any-word retry as well.
func TestBrandFilterKeepsTrapItemsOut(t *testing.T) {
	ref := referenceRaws(t)
	f := newFakeBunnings(t)
	f.brands = []string{"Westward Cement", "Regal Bathroom", "Tusker"}
	f.search = func(q, aq string) []bunnings.Raw {
		return []bunnings.Raw{ref["9100003"], ref["9100005"]}
	}
	cs := f.session(t)
	for _, c := range []struct{ brand, query string }{{"westward", "cement"}, {"regal", "shower seat"}} {
		out := searchTool(t, cs, map[string]any{"query": c.query, "brand": c.brand, "location": "3194"})
		if len(out.Products) != 0 || len(out.BrandsMatched) != 1 || out.ExcludedNotNearby != 2 {
			t.Errorf("brand %s: products %+v, matched %v, excluded %d", c.brand, out.Products, out.BrandsMatched, out.ExcludedNotNearby)
		}
	}
	// Each brand search, and the any-word retry of the two-word one, went
	// out with both the availability clause and the brand clause.
	if len(f.queries) != 3 || !strings.HasPrefix(f.queries[2], "shower OR seat |") {
		t.Fatalf("queries = %q", f.queries)
	}
	for _, q := range f.queries {
		if !strings.Contains(q, `@fulfilmentoptions_0101==("In-Store","Click & Collect")`) || !strings.Contains(q, "NOT @sellername") || !strings.Contains(q, "@brandname==(") {
			t.Errorf("query %q lacks the availability or brand clause", q)
		}
	}
}

// Within a brand, a query whose every word matches nothing is retried once on
// any word, and the result says so.
func TestBrandSearchRetriesOnAnyWord(t *testing.T) {
	ref := referenceRaws(t)
	connector := maps.Clone(ref["9100002"]) // sold In-Store at Northfield
	connector["itemnumber"], connector["name"], connector["brandname"] = json.RawMessage(`"9100006"`),
		json.RawMessage(`"FlowLink-PEX 16 x 20mm Female Connector"`), json.RawMessage(`"FlowLink"`)
	f := newFakeBunnings(t)
	f.brands = []string{"FlowLink", "PipeWolf"}
	f.search = func(q, aq string) []bunnings.Raw {
		if strings.Contains(q, " OR ") {
			return []bunnings.Raw{connector}
		}
		return nil
	}
	out := searchTool(t, f.session(t), map[string]any{"query": "FlowLink PEX 16mm female", "brand": "flowlink", "location": "3194"})
	if !out.MatchedAnyWord || len(out.Products) != 1 || out.Products[0].ItemNumber != "9100006" || !strings.Contains(out.Note, "every word") {
		t.Errorf("out = %+v", out)
	}
	if len(f.queries) != 2 || !strings.HasPrefix(f.queries[0], "PEX 16mm female |") || !strings.Contains(f.queries[0], `@brandname==("FlowLink")`) ||
		!strings.Contains(f.queries[1], " OR ") || strings.Contains(strings.ToLower(f.queries[1]), "flowlink |") {
		t.Errorf("queries = %q", f.queries)
	}

	// A query that is only the brand searches with the brand word, not an
	// empty expression.
	f.queries = nil
	searchTool(t, f.session(t), map[string]any{"query": "FlowLink", "brand": "flowlink", "location": "3194"})
	if len(f.queries) == 0 || !strings.HasPrefix(f.queries[0], "FlowLink |") {
		t.Errorf("brand-only query sent as %q", f.queries)
	}
}
