// Package e2e drives the built server over stdio like a real MCP client, against
// the live Bunnings website, and checks what it returns against independent
// sources: the website's own APIs called separately, and the product page
// rendered in headless Chrome with a store selected.
//
// It is opt-in, because stock and prices change daily and the site is someone
// else's:
//
//	BUNNINGS_LIVE=1 go test -v -timeout 15m ./e2e
//
// The browser comparison needs Node 22+ and Chrome (CHROME overrides the path)
// and is skipped without them.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var binary string

func TestMain(m *testing.M) {
	if os.Getenv("BUNNINGS_LIVE") == "" {
		fmt.Println("skipping live tests; set BUNNINGS_LIVE=1 to run them against bunnings.com.au")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "bunnings-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "snag-mcp")
	if out, err := exec.Command("go", "build", "-o", binary, "../cmd/snag-mcp").CombinedOutput(); err != nil {
		fmt.Printf("build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type server struct {
	s       *mcp.ClientSession
	elicits atomic.Int32
}

// start launches the binary with a fresh config. answer, when set, is what the
// simulated user types into an elicitation prompt; empty means the client
// does not support elicitation at all.
func start(t *testing.T, answer string) *server {
	t.Helper()
	srv := &server{}
	opts := &mcp.ClientOptions{}
	if answer != "" {
		opts.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			srv.elicits.Add(1)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"location": answer}}, nil
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, opts)
	cmd := exec.Command(binary, "-config", filepath.Join(t.TempDir(), "config.json"))
	cmd.Stderr = os.Stderr
	s, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv.s = s
	return srv
}

// call runs a tool and decodes its structured result, failing on a tool error.
func (srv *server) call(t *testing.T, name string, args map[string]any, out any) {
	t.Helper()
	res, err := srv.callRaw(name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s(%v): tool error: %s", name, args, text(res))
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func (srv *server) callRaw(name string, args map[string]any) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return srv.s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Output shapes, declared here rather than imported so the test reads the wire
// format a client sees.
type coverage struct {
	Location       string  `json:"location"`
	RadiusKM       float64 `json:"radius_km"`
	StoresInRadius int     `json:"stores_in_radius"`
	StoresCovered  int     `json:"stores_covered"`
	NearestOutside *struct {
		Name       string  `json:"name"`
		DistanceKM float64 `json:"distance_km"`
	} `json:"nearest_store_outside_radius"`
}

type searchOut struct {
	Products []struct {
		ItemNumber    string  `json:"item_number"`
		Name          string  `json:"name"`
		PriceMin      float64 `json:"price_min"`
		StoresSelling int     `json:"stores_selling"`
	} `json:"products"`
	Coverage coverage `json:"coverage"`
}

type productOut struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	Availability struct {
		SoldNearby bool   `json:"sold_nearby"`
		Reason     string `json:"reason"`
		Nearest    *struct {
			StoreID string  `json:"store_id"`
			Price   float64 `json:"price"`
		} `json:"nearest_store"`
	} `json:"availability"`
	WeightKG *float64 `json:"weight_kg"`
	Packages []struct {
		LengthMM float64 `json:"length_mm"`
	} `json:"packages"`
}

type line struct {
	ItemNumber string   `json:"item_number"`
	Status     string   `json:"status"`
	Stock      *int     `json:"stock"`
	UnitPrice  *float64 `json:"unit_price"`
	Aisle      string   `json:"aisle"`
	Bay        string   `json:"bay"`
}

type listOut struct {
	PickList *struct {
		Store struct {
			ID string `json:"store_id"`
		} `json:"store"`
		Lines []line  `json:"lines"`
		Total float64 `json:"total"`
	} `json:"pick_list"`
	Stores []struct {
		ID       string `json:"store_id"`
		Name     string `json:"name"`
		Fillable int    `json:"lines_in_stock"`
		Missing  []struct {
			ItemNumber string `json:"item_number"`
			Status     string `json:"status"`
		} `json:"missing"`
	} `json:"stores"`
	Load struct {
		Longest *struct {
			ItemNumber string  `json:"item_number"`
			LengthM    float64 `json:"length_m"`
		} `json:"longest_item"`
		WeightUnknown []string `json:"weight_unknown"`
	} `json:"load"`
	Coverage coverage `json:"coverage"`
}

func has(s searchOut, item string) bool {
	for _, p := range s.Products {
		if p.ItemNumber == item {
			return true
		}
	}
	return false
}

// The session a user first has: five tools, no location yet, the client asked
// for one, and it remembered thereafter.
func TestLiveFirstUse(t *testing.T) {
	srv := start(t, "3194")
	tl, err := srv.s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tl.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "check_list,get_product,get_reviews,search_products,set_location" {
		t.Errorf("tools = %v", names)
	}

	var s searchOut
	srv.call(t, "search_products", map[string]any{"query": "drill bit set", "limit": 5}, &s)
	if srv.elicits.Load() != 1 || !strings.Contains(s.Coverage.Location, "3194") {
		t.Fatalf("elicits %d, location %q; want the user asked once and 3194 used", srv.elicits.Load(), s.Coverage.Location)
	}
	if len(s.Products) == 0 || s.Coverage.StoresInRadius < 30 || s.Coverage.RadiusKM != 50 {
		t.Errorf("search = %+v", s)
	}
	srv.call(t, "search_products", map[string]any{"query": "hammer", "limit": 3}, &s)
	if srv.elicits.Load() != 1 {
		t.Error("asked for the location again after it was saved")
	}

	// A client that cannot elicit gets told to ask the user, not a guess.
	bare := start(t, "")
	res, err := bare.callRaw("search_products", map[string]any{"query": "hammer"})
	if err != nil || !res.IsError || !strings.Contains(text(res), "set_location") {
		t.Errorf("without elicitation: err %v, result %s", err, text(res))
	}
}

// A client on an older protocol is asked with a direct elicitation/create
// request mid-call rather than an input request. Spoken in raw JSON-RPC, since
// the SDK's own client always negotiates the newest version.
func TestLiveElicitationOnOlderProtocol(t *testing.T) {
	cmd := exec.Command(binary, "-config", filepath.Join(t.TempDir(), "config.json"))
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Wait() }()
	dec := json.NewDecoder(stdout)
	send := func(m map[string]any) { b, _ := json.Marshal(m); stdin.Write(append(b, '\n')) }
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{"elicitation": map[string]any{}},
		"clientInfo": map[string]any{"name": "old", "version": "1"}}})
	var m map[string]any
	dec.Decode(&m)
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "search_products", "arguments": map[string]any{"query": "hammer", "limit": 2}}})
	asked := false
	for {
		m = nil
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m["method"] == "elicitation/create" {
			asked = true
			send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{
				"action": "accept", "content": map[string]any{"location": "6530"}}})
			continue
		}
		if m["id"] == float64(2) {
			break
		}
	}
	res, _ := json.Marshal(m["result"])
	if !asked || !strings.Contains(string(res), "Geraldton WA 6530") {
		t.Errorf("asked=%v result=%.300s", asked, res)
	}
}

// The invariants: a WA-only cement and a marketplace shower seat never reach a
// Melbourne shopper, and the cement does reach a Perth one.
func TestLiveExclusions(t *testing.T) {
	srv := start(t, "")
	var s searchOut
	srv.call(t, "set_location", map[string]any{"postcode_or_suburb": "3194"}, &map[string]any{})

	for _, q := range []string{"swan cement", "swan 20kg general purpose grey cement", "cement 20kg"} {
		srv.call(t, "search_products", map[string]any{"query": q, "limit": 25}, &s)
		if has(s, "0760298") {
			t.Errorf("3194 search %q returned Swan cement 0760298", q)
		}
	}
	for _, q := range []string{"king shower seat", "shower seat with back and armrests", "shower seat"} {
		srv.call(t, "search_products", map[string]any{"query": q, "limit": 25}, &s)
		if has(s, "1151723") {
			t.Errorf("3194 search %q returned marketplace shower seat 1151723", q)
		}
	}
	var p productOut
	srv.call(t, "get_product", map[string]any{"item_number": "0760298"}, &p)
	if p.Availability.SoldNearby || p.Availability.Nearest != nil {
		t.Errorf("get_product 0760298 at 3194 = %+v; want not sold nearby and no price", p.Availability)
	}
	srv.call(t, "get_product", map[string]any{"item_number": "1151723"}, &p)
	if p.Availability.SoldNearby || p.Availability.Reason != "marketplace" {
		t.Errorf("get_product 1151723 = %+v; want marketplace", p.Availability)
	}
	var l listOut
	srv.call(t, "check_list", map[string]any{"items": []map[string]any{
		{"item_number": "0563516"}, {"item_number": "0760298", "quantity": 2}, {"item_number": "1151723"},
	}}, &l)
	for _, st := range l.Stores {
		for _, m := range st.Missing {
			want := map[string]string{"0760298": "not_sold_here", "1151723": "marketplace"}[m.ItemNumber]
			if want != "" && m.Status != want {
				t.Errorf("%s at %s: %s, want %s", m.ItemNumber, st.Name, m.Status, want)
			}
		}
		if st.Fillable > 1 {
			t.Errorf("%s fills %d lines; only the drill bits are buyable here", st.Name, st.Fillable)
		}
	}

	// Perth.
	srv.call(t, "search_products", map[string]any{"query": "swan cement", "location": "6000", "limit": 25}, &s)
	if !has(s, "0760298") {
		t.Errorf("6000 search for swan cement did not return 0760298: %+v", s.Products)
	}
	srv.call(t, "get_product", map[string]any{"item_number": "0760298", "location": "6000"}, &p)
	if !p.Availability.SoldNearby || p.Availability.Nearest == nil || p.Availability.Nearest.Price < 5 {
		t.Errorf("get_product 0760298 at 6000 = %+v; want sold with a real price", p.Availability)
	}
	srv.call(t, "search_products", map[string]any{"query": "king shower seat", "location": "6000", "limit": 25}, &s)
	if has(s, "1151723") {
		t.Error("6000 search returned marketplace shower seat 1151723")
	}
}

// Somewhere with no store in reach names the nearest rather than widening on
// its own.
func TestLiveNoStoreInRadius(t *testing.T) {
	srv := start(t, "")
	var s searchOut
	srv.call(t, "search_products", map[string]any{"query": "hammer", "location": "Birdsville QLD"}, &s)
	if s.Coverage.StoresInRadius != 0 || len(s.Products) != 0 || s.Coverage.NearestOutside == nil || s.Coverage.NearestOutside.DistanceKM < 100 {
		t.Errorf("Birdsville = %+v", s.Coverage)
	}
	t.Logf("Birdsville: nearest store %s, %.0f km", s.Coverage.NearestOutside.Name, s.Coverage.NearestOutside.DistanceKM)
}

// What the server says about an item at a store, against what the website
// says: its APIs called separately, and the product page rendered in Chrome.
func TestLiveMatchesSite(t *testing.T) {
	cases := []struct {
		item, location, browserPostcode, store, storeName string
	}{
		{"0563516", "3194", "3194", "6400", "Mentone"},
		{"8032174", "3194", "3194", "6400", "Mentone"},
		{"0616147", "3194", "3194", "6400", "Mentone"}, // special order
		{"8032174", "3194", "3194", "6363", "Moorabbin"},
		{"0563516", "6000", "6008", "2260", "Subiaco"},
		{"8032174", "6000", "6008", "2260", "Subiaco"},
		{"0760298", "6000", "6008", "2260", "Subiaco"}, // WA-only cement
	}
	srv := start(t, "")
	ref := newSite(t)
	node, chrome := browserAvailable()
	if !chrome {
		t.Log("Chrome or Node 22+ not found; comparing with the website's APIs only")
	}

	t.Logf("%-8s %-10s | %-26s | %-26s | %-26s", "item", "store", "MCP price/stock/aisle", "site API", "browser page")
	for _, c := range cases {
		var l listOut
		srv.call(t, "check_list", map[string]any{"items": []map[string]any{{"item_number": c.item}},
			"store": c.store, "location": c.location}, &l)
		if l.PickList == nil || len(l.PickList.Lines) != 1 || l.PickList.Store.ID != c.store {
			t.Fatalf("%s at %s: no pick list: %+v", c.item, c.store, l)
		}
		got := l.PickList.Lines[0]
		want := ref.view(t, c.item, c.store)

		var br *browserView
		if chrome {
			var p productOut
			srv.call(t, "get_product", map[string]any{"item_number": c.item, "location": c.location}, &p)
			br = renderPage(t, node, p.URL, c.browserPostcode, c.storeName, c.store)
		}
		t.Logf("%-8s %-10s | %-26s | %-26s | %-26s", c.item, c.storeName,
			fmt.Sprintf("$%s %s %s", money(got.UnitPrice), count(got.Stock, got.Status), aisle(got.Aisle, got.Bay)),
			fmt.Sprintf("$%.2f %s %s", want.Price, count(want.Stock, special(want.Special)), aisle(want.Aisle, want.Bay)),
			br.String())

		if got.UnitPrice == nil || math.Abs(*got.UnitPrice-want.Price) > 0.005 {
			t.Errorf("%s at %s: MCP price %s, site priceInfo %.2f", c.item, c.storeName, money(got.UnitPrice), want.Price)
		}
		if got.Aisle != want.Aisle || got.Bay != want.Bay {
			t.Errorf("%s at %s: MCP aisle %s, site %s", c.item, c.storeName, aisle(got.Aisle, got.Bay), aisle(want.Aisle, want.Bay))
		}
		if want.Special {
			if got.Status != "special_order" || got.Stock != nil {
				t.Errorf("%s at %s: site says special order, MCP says %s", c.item, c.storeName, got.Status)
			}
		} else if !closeStock(got.Stock, want.Stock) {
			t.Errorf("%s at %s: MCP stock %s, site %s", c.item, c.storeName, count(got.Stock, ""), count(want.Stock, ""))
		}
		if br == nil {
			continue
		}
		if br.Price == nil || math.Abs(*br.Price-*got.UnitPrice) > 0.005 {
			t.Errorf("%s at %s: browser shows %q, MCP $%s", c.item, c.storeName, br.PriceText, money(got.UnitPrice))
		}
		if br.Aisle != "" && (br.Aisle != got.Aisle || br.Bay != got.Bay) {
			t.Errorf("%s at %s: browser shows %s, MCP %s", c.item, c.storeName, aisle(br.Aisle, br.Bay), aisle(got.Aisle, got.Bay))
		}
		if !want.Special && !closeStock(got.Stock, br.Stock) {
			t.Errorf("%s at %s: browser's stock %s, MCP %s", c.item, c.storeName, count(br.Stock, ""), count(got.Stock, ""))
		}
		if !strings.Contains(br.Store, c.storeName) {
			t.Errorf("browser ended on store %q, want %s", br.Store, c.storeName)
		}
	}
}

// closeStock allows for stock moving between two reads a few seconds apart.
func closeStock(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	d := math.Abs(float64(*a - *b))
	return d <= math.Max(3, 0.1*float64(max(*a, *b)))
}

type browserView struct {
	Store     string
	PriceText string
	Price     *float64
	InStore   string
	Aisle     string
	Bay       string
	Stock     *int
}

func (b *browserView) String() string {
	if b == nil {
		return "(no browser)"
	}
	return fmt.Sprintf("%s %s %s", b.PriceText, count(b.Stock, ""), aisle(b.Aisle, b.Bay))
}

func browserAvailable() (string, bool) {
	node, err := exec.LookPath("node")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(node, "--version").Output()
	if v := regexp.MustCompile(`^v(\d+)`).FindSubmatch(out); err != nil || v == nil {
		return "", false
	} else if n, _ := strconv.Atoi(string(v[1])); n < 22 {
		return "", false
	}
	chrome := os.Getenv("CHROME")
	if chrome == "" {
		chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	_, err = os.Stat(chrome)
	return node, err == nil
}

func renderPage(t *testing.T, node, url, postcode, store, storeID string) *browserView {
	t.Helper()
	var out []byte
	var err error
	// The page is someone else's and occasionally slow; one more go, no more.
	for attempt := 0; attempt < 2; attempt++ {
		if out, err = exec.Command(node, "browser/product.mjs", url, postcode, store, storeID).Output(); err == nil {
			break
		}
	}
	if err != nil {
		t.Errorf("browser %s at %s: %v", url, store, err)
		return nil
	}
	var r struct {
		Store     string `json:"store"`
		PriceText string `json:"price_text"`
		InStore   string `json:"in_store_text"`
		Aisle     string `json:"aisle"`
		Bay       string `json:"bay"`
		API       struct {
			Fulfillment struct {
				InStore struct {
					Stock *int `json:"stock"`
				} `json:"inStorePickUpData"`
			} `json:"fulfillment"`
		} `json:"api"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Errorf("browser output: %v: %s", err, out)
		return nil
	}
	b := &browserView{Store: r.Store, PriceText: r.PriceText, InStore: r.InStore, Aisle: r.Aisle, Bay: r.Bay,
		Stock: r.API.Fulfillment.InStore.Stock}
	if f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(r.PriceText, "$"), ",", ""), 64); err == nil {
		b.Price = &f
	}
	return b
}

func money(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *p)
}

func count(s *int, status string) string {
	if s == nil {
		if status == "" {
			status = "no-count"
		}
		return status
	}
	return fmt.Sprintf("%d", *s)
}

func aisle(a, b string) string {
	if a == "" {
		return "no-aisle"
	}
	return a + "/" + b
}

func special(b bool) string {
	if b {
		return "special_order"
	}
	return ""
}
