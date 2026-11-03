package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------- documents and connection details ----------

type productDocs struct {
	Specs []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"specs"`
	ConnectionDetails []struct {
		Source string `json:"source"`
		Text   string `json:"text"`
	} `json:"connection_details"`
	ConnectionNote string `json:"connection_note"`
	Documents      []struct {
		Type      string `json:"type"`
		URL       string `json:"url"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"documents"`
	DocumentsNote string `json:"documents_note"`
}

func TestLiveDocuments(t *testing.T) {
	srv := start(t, "")
	var p productDocs
	srv.call(t, "get_product", map[string]any{"item_number": "4950096", "location": "3194"}, &p)
	if len(p.Documents) == 0 || p.Documents[0].Type != "Installation" || !strings.HasPrefix(p.Documents[0].URL, "https://") {
		t.Fatalf("SmarteX PEX 4950096 documents = %+v", p.Documents)
	}
	// The link is real: Bunnings' media server hands over a PDF.
	resp, err := http.Get(p.Documents[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 5)
	io.ReadFull(resp.Body, head)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(head) != "%PDF-" {
		t.Errorf("%s: HTTP %d, starts %q", p.Documents[0].URL, resp.StatusCode, head)
	}
	t.Logf("4950096: %s %s (%d bytes)", p.Documents[0].Type, p.Documents[0].URL, p.Documents[0].SizeBytes)

	p = productDocs{}
	srv.call(t, "get_product", map[string]any{"item_number": "0571978", "location": "3194"}, &p)
	if len(p.Documents) != 0 || p.DocumentsNote == "" {
		t.Errorf("Brasshards 0571978: documents %+v, note %q; want none, said so", p.Documents, p.DocumentsNote)
	}
	// Brasshards gives its thread only in the name ("15C x 15MI").
	if len(p.ConnectionDetails) == 0 {
		t.Errorf("0571978 connection details empty: %q", p.ConnectionNote)
	}
	t.Logf("0571978: %+v", p.ConnectionDetails)

	p = productDocs{}
	srv.call(t, "get_product", map[string]any{"item_number": "3130065", "location": "3194"}, &p)
	joined := ""
	for _, c := range p.ConnectionDetails {
		joined += c.Text + " | "
	}
	if !strings.Contains(joined, "BSP") {
		t.Errorf("Pope 3130065 connection details %q; want the BSP sizes it states", joined)
	}
	t.Logf("3130065: %s", joined)
}

// ---------- reviews and questions ----------

type reviewsOut struct {
	ReviewCount int `json:"review_count"`
	Reviews     []struct {
		Rating  int    `json:"rating"`
		Text    string `json:"text"`
		Date    string `json:"date"`
		Helpful int    `json:"helpful_votes"`
	} `json:"reviews"`
	QuestionCount int `json:"question_count"`
	Questions     []struct {
		Question string `json:"question"`
		Answers  []struct {
			Text string `json:"text"`
		} `json:"answers"`
	} `json:"questions"`
}

func TestLiveReviews(t *testing.T) {
	srv := start(t, "")
	var r reviewsOut
	srv.call(t, "get_reviews", map[string]any{"item_number": "3129102", "limit": 10}, &r)
	if r.ReviewCount < 15 || len(r.Reviews) < 5 {
		t.Fatalf("Pope 3129102: %d reviews, %d returned", r.ReviewCount, len(r.Reviews))
	}
	for i, x := range r.Reviews {
		if x.Text == "" || x.Rating < 1 || x.Rating > 5 {
			t.Errorf("review %d = %+v", i, x)
		}
		if i > 0 && x.Helpful > r.Reviews[i-1].Helpful {
			t.Errorf("most helpful first: review %d has %d votes after %d", i, x.Helpful, r.Reviews[i-1].Helpful)
		}
	}
	// Against Bazaarvoice directly, with the key read from its config here.
	if want := bvCount(t, "reviews", "3129102"); want < r.ReviewCount || want > r.ReviewCount+19 {
		t.Errorf("review_count %d, Bazaarvoice has %d reviews (with and without text)", r.ReviewCount, want)
	}
	r = reviewsOut{}
	srv.call(t, "get_reviews", map[string]any{"item_number": "3129102", "sort": "recent", "limit": 5}, &r)
	for i := 1; i < len(r.Reviews); i++ {
		if r.Reviews[i].Date > r.Reviews[i-1].Date {
			t.Errorf("recent first: %s after %s", r.Reviews[i].Date, r.Reviews[i-1].Date)
		}
	}

	r = reviewsOut{}
	srv.call(t, "get_reviews", map[string]any{"item_number": "0808187"}, &r)
	if r.QuestionCount < 1 || len(r.Questions) < 1 || len(r.Questions[0].Answers) < 1 || r.Questions[0].Answers[0].Text == "" {
		t.Errorf("HP 61XL 0808187 questions = %d %+v", r.QuestionCount, r.Questions)
	}
	if want := bvCount(t, "questions", "0808187"); want != r.QuestionCount {
		t.Errorf("question_count %d, Bazaarvoice %d", r.QuestionCount, want)
	}
}

// bvCount asks Bazaarvoice how many reviews or questions an item has, with
// the key read from the widget config by this test, not the server.
func bvCount(t *testing.T, kind, item string) int {
	t.Helper()
	get := func(u string) []byte {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	cfg := get("https://apps.bazaarvoice.com/deployments/bunnings-nz/main_site/production/en_AU/ratings-config.js")
	m := regexp.MustCompile(`"mprAPIKey":"(\w+)"`).FindSubmatch(cfg)
	if m == nil {
		t.Fatal("no mprAPIKey in the Bazaarvoice config")
	}
	var out struct{ TotalResults int }
	json.Unmarshal(get(fmt.Sprintf("https://api.bazaarvoice.com/data/%s.json?apiversion=5.4&passkey=%s&Filter=ProductId:%s&Limit=1", kind, m[1], item)), &out)
	return out.TotalResults
}

// ---------- brand filter ----------

type brandOut struct {
	Products []struct {
		ItemNumber string `json:"item_number"`
		Name       string `json:"name"`
		Brand      string `json:"brand"`
	} `json:"products"`
	BrandsMatched    []string `json:"brands_matched"`
	BrandSuggestions []string `json:"brand_suggestions"`
	BrandsForQuery   []struct {
		Value string `json:"value"`
	} `json:"brands_for_query"`
}

func TestLiveBrandFilter(t *testing.T) {
	srv := start(t, "")
	var b brandOut
	// The feedback that prompted it: this search used to return SharkBite.
	srv.call(t, "search_products", map[string]any{"query": "PEX 16mm female", "brand": "smartex", "location": "3194", "limit": 10}, &b)
	if len(b.Products) == 0 || !slices.Equal(b.BrandsMatched, []string{"SmarteX"}) {
		t.Fatalf("smartex: matched %v, %d products", b.BrandsMatched, len(b.Products))
	}
	for _, p := range b.Products {
		if p.Brand != "SmarteX" {
			t.Errorf("brand smartex returned %s %s (%s)", p.ItemNumber, p.Name, p.Brand)
		}
	}
	t.Logf("smartex: %d products, first %s %s", len(b.Products), b.Products[0].ItemNumber, b.Products[0].Name)

	b = brandOut{}
	srv.call(t, "search_products", map[string]any{"query": "push fit elbow", "brand": "sharkbyte", "location": "3194", "limit": 5}, &b)
	if !slices.Contains(b.BrandsMatched, "SharkBite") || len(b.Products) == 0 {
		t.Errorf("sharkbyte: matched %v, %d products", b.BrandsMatched, len(b.Products))
	}

	b = brandOut{}
	srv.call(t, "search_products", map[string]any{"query": "pex", "brand": "zzqqxx", "location": "3194"}, &b)
	if len(b.Products) != 0 || len(b.BrandsForQuery) == 0 {
		t.Errorf("nonsense brand: %d products, brands for query %v", len(b.Products), b.BrandsForQuery)
	}

	// The exclusions hold with a brand filter: the brand of a WA-only or a
	// marketplace item is not a brand sold near Melbourne.
	b = brandOut{}
	srv.call(t, "search_products", map[string]any{"query": "cement", "brand": "swan", "location": "3194", "limit": 25}, &b)
	for _, p := range b.Products {
		if p.ItemNumber == "0760298" {
			t.Error("brand swan at 3194 returned Swan cement 0760298")
		}
	}
	t.Logf("swan at 3194: matched %v, %d products", b.BrandsMatched, len(b.Products))
	b = brandOut{}
	srv.call(t, "search_products", map[string]any{"query": "shower seat", "brand": "king", "location": "3194", "limit": 25}, &b)
	for _, p := range b.Products {
		if p.ItemNumber == "1151723" {
			t.Error("brand king at 3194 returned marketplace shower seat 1151723")
		}
	}
	b = brandOut{}
	srv.call(t, "search_products", map[string]any{"query": "cement", "brand": "swan", "location": "6000", "limit": 25}, &b)
	found := false
	for _, p := range b.Products {
		found = found || p.ItemNumber == "0760298"
	}
	if !found {
		t.Errorf("brand swan at 6000 did not return 0760298: matched %v, %+v", b.BrandsMatched, b.Products)
	}
}

// ---------- store map ----------

type mapListOut struct {
	PickList *struct {
		Lines []struct {
			ItemNumber string `json:"item_number"`
			Status     string `json:"status"`
			Aisle      string `json:"aisle"`
			Bay        string `json:"bay"`
			Stop       int    `json:"stop"`
		} `json:"lines"`
	} `json:"pick_list"`
	StoreMap *struct {
		MapID    string   `json:"map_id"`
		Ordering string   `json:"ordering"`
		WalkM    *float64 `json:"walk_m"`
		Start    *struct {
			Name string `json:"name"`
		} `json:"start"`
		Storeys []struct {
			WidthM  float64 `json:"width_m"`
			HeightM float64 `json:"height_m"`
		} `json:"storeys"`
		Stops []struct {
			Stop   int      `json:"stop"`
			Aisle  int      `json:"aisle"`
			Bay    int      `json:"bay"`
			Storey int      `json:"storey"`
			X      float64  `json:"x_m"`
			Y      float64  `json:"y_m"`
			Items  []string `json:"item_numbers"`
		} `json:"stops"`
		NotOnMap []struct {
			ItemNumber string `json:"item_number"`
			Reason     string `json:"reason"`
		} `json:"not_on_map"`
		Image bool `json:"image_attached"`
	} `json:"store_map"`
	MapNote string `json:"map_note"`
}

// The pick list for one store comes with its floor plan: each line to pick
// up numbered in walking order, the numbers in the list matching the map's,
// and the bays where the website's own location lookup puts them.
func TestLiveStoreMap(t *testing.T) {
	cases := []struct {
		store, name, location string
		items                 []string
		notSold               string // on the list, not sold at this store
	}{
		{"6400", "Mentone", "3194", []string{"0563516", "8032174", "4950096", "3129102", "0760298"}, "0760298"},
		{"2260", "Subiaco", "6000", []string{"0563516", "8032174", "0760298", "1151723"}, "1151723"},
	}
	srv := start(t, "")
	ref := newSite(t)
	manifest := viewerManifest(t)
	outDir := os.Getenv("BUNNINGS_E2E_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	for _, c := range cases {
		var items []map[string]any
		for _, it := range c.items {
			items = append(items, map[string]any{"item_number": it, "quantity": 2})
		}
		args := map[string]any{"items": items, "store": c.store, "location": c.location}
		res, err := srv.callRaw("check_list", args)
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", c.name, err, text(res))
		}
		var l mapListOut
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &l)
		sm := l.StoreMap
		if sm == nil || l.PickList == nil {
			t.Fatalf("%s: no store map (%s)", c.name, l.MapNote)
		}
		if sm.MapID != manifest[c.store] || sm.Ordering != "walking_graph" || sm.WalkM == nil || sm.Start == nil || !sm.Image {
			t.Errorf("%s: map %q (viewer lists %q), ordering %s, start %v, image %v", c.name, sm.MapID, manifest[c.store], sm.Ordering, sm.Start, sm.Image)
		}

		// The image is a real PNG of a sensible size.
		var img []byte
		for _, ct := range res.Content {
			if ic, ok := ct.(*mcp.ImageContent); ok && ic.MIMEType == "image/png" {
				img = ic.Data
			}
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(img))
		if err != nil || cfg.Width < 800 || cfg.Height < 300 {
			t.Errorf("%s: image %dx%d, %v", c.name, cfg.Width, cfg.Height, err)
		}
		file := filepath.Join(outDir, fmt.Sprintf("map-%s.png", c.store))
		os.WriteFile(file, img, 0o644)

		// List order is walking order: numbered lines first, 1, 2, 3...,
		// each number's lines at that stop's bay.
		prev := 0
		stopAt := map[int][2]int{}
		for _, s := range sm.Stops {
			stopAt[s.Stop] = [2]int{s.Aisle, s.Bay}
			if s.X < 0 || s.Y < 0 || s.X > sm.Storeys[s.Storey-1].WidthM || s.Y > sm.Storeys[s.Storey-1].HeightM {
				t.Errorf("%s: stop %d at %.1f,%.1f outside the storey", c.name, s.Stop, s.X, s.Y)
			}
		}
		var order []string
		for _, ln := range l.PickList.Lines {
			order = append(order, fmt.Sprintf("%d:%s@%s/%s", ln.Stop, ln.ItemNumber, ln.Aisle, ln.Bay))
			if ln.Stop == 0 {
				prev = 1 << 30
				continue
			}
			if ln.Stop < prev || ln.Stop > prev+1 {
				t.Errorf("%s: list order %v is not walking order", c.name, order)
			}
			prev = ln.Stop
			if fmt.Sprintf("%d/%d", stopAt[ln.Stop][0], stopAt[ln.Stop][1]) != strings.TrimLeft(ln.Aisle, "0")+"/"+strings.TrimLeft(ln.Bay, "0") {
				t.Errorf("%s: %s at %s/%s numbered %d, which is bay %v", c.name, ln.ItemNumber, ln.Aisle, ln.Bay, ln.Stop, stopAt[ln.Stop])
			}
			// The bay is where the website's own lookup says the item is.
			if w := ref.view(t, ln.ItemNumber, c.store); w.Aisle != ln.Aisle || w.Bay != ln.Bay {
				t.Errorf("%s: %s on the map at %s/%s, website says %s/%s", c.name, ln.ItemNumber, ln.Aisle, ln.Bay, w.Aisle, w.Bay)
			}
		}
		var off []string
		listed := false
		for _, o := range sm.NotOnMap {
			off = append(off, o.ItemNumber+":"+o.Reason)
			listed = listed || o.ItemNumber == c.notSold
		}
		if !listed {
			t.Errorf("%s: %s should be listed as not on the map: %v", c.name, c.notSold, off)
		}
		summary := res.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(summary, "Walking order from") || !strings.Contains(summary, "\n1. ") {
			t.Errorf("%s: text summary has no walking order:\n%s", c.name, summary)
		}
		t.Logf("%s (%s): %s, %.0f m from %s; list %v; not on map %v; image %dx%d at %s",
			c.name, sm.MapID, sm.Ordering, *sm.WalkM, sm.Start.Name, order, off, cfg.Width, cfg.Height, file)

		// Map off: aisle order, no image, nothing about a walk.
		args["map"] = false
		res, _ = srv.callRaw("check_list", args)
		for _, ct := range res.Content {
			if _, ok := ct.(*mcp.ImageContent); ok {
				t.Errorf("%s: map:false still attached an image", c.name)
			}
		}
		var off2 mapListOut
		b, _ = json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &off2)
		if off2.StoreMap != nil || off2.PickList == nil || off2.PickList.Lines[0].Stop != 0 {
			t.Errorf("%s: map:false gave %+v", c.name, off2.StoreMap)
		}
	}
}

// viewerManifest reads Bunnings' map viewer's store list directly.
func viewerManifest(t *testing.T) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://map-viewer.apps.bunnings.com.au/api/stores-manifest.json", nil)
	req.Header.Set("User-Agent", siteUA)
	req.Header.Set("Referer", "https://map-viewer.apps.bunnings.com.au/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m struct {
		Stores map[string]struct {
			MapID string `json:"mapId"`
		} `json:"stores"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("viewer manifest: %v", err)
	}
	out := map[string]string{}
	for k, v := range m.Stores {
		out[k] = v.MapID
	}
	return out
}

// Hawthorn is on three floors. The text pick list, all a client without
// images has, names each stop's floor and says where the walk changes floor;
// the floor it names is the one the structured map puts the stop on.
func TestLiveStoreMapFloors(t *testing.T) {
	srv := start(t, "")
	items := []map[string]any{{"item_number": "0563516"}, {"item_number": "3129102"}, {"item_number": "0548292"}, {"item_number": "8032174"}}
	res, err := srv.callRaw("check_list", map[string]any{"items": items, "store": "6299", "location": "3122"})
	if err != nil || res.IsError {
		t.Fatalf("%v %s", err, text(res))
	}
	var l struct {
		StoreMap struct {
			Storeys []struct {
				Number int    `json:"number"`
				Name   string `json:"name"`
			} `json:"storeys"`
			Stops []struct {
				Stop   int `json:"stop"`
				Storey int `json:"storey"`
			} `json:"stops"`
		} `json:"store_map"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &l)
	summary := res.Content[0].(*mcp.TextContent).Text
	t.Logf("Hawthorn:\n%s", summary)
	if len(l.StoreMap.Storeys) < 3 || len(l.StoreMap.Stops) == 0 {
		t.Fatalf("store map = %+v", l.StoreMap)
	}
	name := map[int]string{}
	for _, s := range l.StoreMap.Storeys {
		name[s.Number] = s.Name
	}
	floors := map[int]bool{}
	for _, s := range l.StoreMap.Stops {
		floors[s.Storey] = true
		if want := fmt.Sprintf("\n%d. %s: ", s.Stop, name[s.Storey]); !strings.Contains(summary, want) {
			t.Errorf("no line %q", strings.TrimSpace(want))
		}
	}
	if len(floors) > 1 && !regexp.MustCompile(`\nTake the [a-z ]+ to [A-Za-z0-9 ]+\.\n`).MatchString(summary) {
		t.Error("the walk changes floor but the text never says so")
	}
}
