package tools

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensantoso/snag-mcp/internal/bunnings"

	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/kensantoso/snag-mcp/internal/storemap"
)

// exampleStore loads the invented two-storey Example Store from the storemap
// package's test data.
func exampleStore(t *testing.T) *loadedMap {
	t.Helper()
	read := func(name string) []byte {
		f, err := os.Open("../storemap/testdata/0001-" + name + ".json.gz")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(z)
		return b
	}
	m, err := storemap.ParseMap(read("map"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := storemap.ParseGraph(read("nodes"), read("edges"))
	if err != nil {
		t.Fatal(err)
	}
	return &loadedMap{Map: m, Graph: g}
}

func line(item, status, aisle, bay string) shop.Line {
	p, n := 1.0, 3
	l := shop.Line{ItemNumber: item, Name: "item " + item, Quantity: 1, Status: status, Aisle: aisle, Bay: bay}
	if status != shop.NotSoldHere {
		l.UnitPrice, l.LineTotal = &p, &p
	}
	if status == shop.InStock {
		l.Stock = &n
	}
	return l
}

func TestWalkLines(t *testing.T) {
	lm := exampleStore(t)
	// As the item-location API gives them: strings, sometimes zero-padded.
	lines := []shop.Line{
		line("1000001", shop.InStock, "2", "1"),           // Ground
		line("1000002", shop.InStock, "10", "003"),        // Lower Ground
		line("1000003", shop.Short, "02", "01"),           // same bay as the first
		line("1000004", shop.SpecialOrder, "4", "2"),      // nothing to pick up
		line("1000005", shop.OutOfStock, "4", "2"),        // none there
		line("1000006", shop.NotSoldHere, "", ""),         // not sold
		line("1000007", shop.StockUnknown, "", ""),        // sold, no aisle given
		line("1000008", shop.InStock, "99", "1"),          // no such bay on the map
		line("1000009", shop.InStock, "Timber Yard", "1"), // not a number
		line("1000010", shop.StockUnknown, "4", "2"),      // Ground
	}
	shop.SortPickList(lines)
	w, off := walkLines(lines, lm)

	if w.Method != storemap.ByWalkingGraph || len(w.Stops) != 3 {
		t.Fatalf("%s with %d stops", w.Method, len(w.Stops))
	}
	stop := map[string]int{}
	for _, l := range lines {
		stop[l.ItemNumber] = l.Stop
	}
	if stop["1000001"] == 0 || stop["1000001"] != stop["1000003"] {
		t.Errorf("lines in the same bay should share a stop: %v", stop)
	}
	// The list is in walking order, numbered as the walk is, stops first.
	var got []int
	for _, l := range lines {
		got = append(got, l.Stop)
	}
	if !slices.IsSorted(got[:4]) || got[0] != 1 || got[3] != 3 || slices.ContainsFunc(got[4:], func(n int) bool { return n != 0 }) {
		t.Errorf("stops in list order = %v", got)
	}
	for i, s := range w.Stops {
		for _, l := range lines {
			if l.Stop == i+1 {
				a, _ := parseAddr(l.Aisle, l.Bay)
				if a != s.Bay.Addr {
					t.Errorf("line %s at %v numbered %d, which is %v", l.ItemNumber, a, l.Stop, s.Bay.Addr)
				}
			}
		}
	}
	reasons := map[string]string{}
	for _, o := range off {
		reasons[o.ItemNumber] = o.Reason
	}
	want := map[string]string{
		"1000004": "special_order", "1000005": "out_of_stock", "1000006": "not_sold_here",
		"1000007": "no_location", "1000008": "location_not_on_map", "1000009": "location_not_on_map",
	}
	for k, v := range want {
		if reasons[k] != v {
			t.Errorf("%s: reason %q, want %q", k, reasons[k], v)
		}
	}
	if len(off) != len(want) {
		t.Errorf("not on map: %v", off)
	}

	out := mapOut(lm, w, lines, off)
	if out.Ordering != "walking_graph" || out.WalkM == nil || len(out.Stops) != 3 || len(out.Storeys) != 2 {
		t.Fatalf("map out = %+v", out)
	}
	k := slices.IndexFunc(out.Stops, func(s mapStop) bool { return s.Address == "002.001" })
	if k < 0 || !slices.Equal(out.Stops[k].Items, []string{"1000001", "1000003"}) || out.Stops[k].Stop != k+1 {
		t.Errorf("stops = %+v", out.Stops)
	}
	// Metres from the top left of the storey, y down: Example Store's entrance is
	// at the top right of Ground, as on its printed map.
	g := out.Storeys[1]
	if out.Start == nil || out.Start.Storey != 2 || out.Start.X < 0.7*g.WidthM || out.Start.Y > 0.3*g.HeightM {
		t.Errorf("start = %+v on %vx%v m", out.Start, g.WidthM, g.HeightM)
	}
	for _, s := range out.Stops {
		st := out.Storeys[s.Storey-1]
		if s.X < 0 || s.Y < 0 || s.X > st.WidthM || s.Y > st.HeightM || s.LegM == nil {
			t.Errorf("stop %+v outside %vx%v m", s, st.WidthM, st.HeightM)
		}
	}
}

// Why stops fall back to straight lines is said, not guessed at.
func TestStraightLineWhy(t *testing.T) {
	lm := exampleStore(t)
	bays := []storemap.Located{}
	if l, ok := lm.Map.Find(storemap.Addr{Aisle: 2, Bay: 1}); ok {
		bays = append(bays, l)
	}
	walk := func(g *storemap.Graph) storemap.Walk { return storemap.Plan(lm.Map, g, bays) }
	if why := straightLineWhy(lm, walk(lm.Graph)); why != "" {
		t.Errorf("walking graph used, but: %q", why)
	}
	_, err := storemap.ParseGraph([]byte(`{`), []byte(`{}`))
	for _, c := range []struct {
		lm   *loadedMap
		want string
	}{
		{&loadedMap{Map: lm.Map}, "Bunnings publishes no walking graph for this store"},
		{&loadedMap{Map: lm.Map, GraphErr: err}, "the store's walking graph could not be read (walking graph"},
	} {
		if why := straightLineWhy(c.lm, walk(nil)); !strings.HasPrefix(why, c.want) {
			t.Errorf("why = %q, want %q", why, c.want)
		}
	}
}

func TestWalkLinesNothingToPick(t *testing.T) {
	lines := []shop.Line{line("1000006", shop.NotSoldHere, "", ""), line("1000004", shop.SpecialOrder, "4", "2")}
	w, off := walkLines(lines, exampleStore(t))
	if len(w.Stops) != 0 || len(off) != 2 || lines[0].Stop != 0 || lines[1].Stop != 0 {
		t.Errorf("stops %v, off %v", w.Stops, off)
	}
}

// In a store on two floors the text names each stop's floor and says where
// to change floor; the text is all a client without images has.
func TestPickSummaryFloors(t *testing.T) {
	lm := exampleStore(t)
	lines := []shop.Line{
		line("1000001", shop.InStock, "8", "5"),   // Ground, below the entrance and far from the registers
		line("1000002", shop.InStock, "10", "3"),  // Lower Ground
		line("1000003", shop.InStock, "21", "1"),  // Lower Ground
		line("1000006", shop.NotSoldHere, "", ""), // not on the map
	}
	shop.SortPickList(lines)
	w, _ := walkLines(lines, lm)
	pl := &pickList{Store: shop.Store{ID: "0001", Name: "Example Store"}, Lines: lines}
	got := pickSummary(pl, shop.Load{}, &w, lm.Map, "", true)
	want := []string{
		"Walking order from Main Entrance (Ground) to the registers",
		"\n1. Ground: 1000001 x1",
		"\nTake the travelator to Lower Ground.\n2. Lower Ground: ",
		"\n3. Lower Ground: ",
		"\nThen take the travelator back to Ground for the registers.\n- (not on map) 1000006",
	}
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}

	// A map may name each end of a link differently; the text names both.
	for i := range lm.Map.Storey(1).LevelAccess {
		if lm.Map.Storey(1).LevelAccess[i].Name == "Travelator" {
			lm.Map.Storey(1).LevelAccess[i].Name = "Ramp"
		}
	}
	got = pickSummary(pl, shop.Load{}, &w, lm.Map, "", true)
	for _, s := range []string{"\nTake the travelator or ramp to Lower Ground.\n", "\nThen take the ramp or travelator back to Ground"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}

	// One floor's walk in a one-floor store: no floor names, no changes.
	one := *lm.Map
	one.Storeys = []*storemap.Storey{lm.Map.Storey(2)}
	got = pickSummary(pl, shop.Load{}, &storemap.Walk{Start: w.Start, StartStorey: 2, Stops: w.Stops[:1], End: w.End}, &one, "", false)
	if strings.Contains(got, "Ground") || strings.Contains(got, "Take the") || !strings.Contains(got, "\n1. 1000001 x1") {
		t.Errorf("single floor:\n%s", got)
	}
}

// Several pick lists for one store at once download its plan once.
func TestStoreMapLoadsOnce(t *testing.T) {
	files := map[string][]byte{"/api/stores-manifest.json": []byte(`{"stores":{"0001":{"mapId":"0001-0001"}}}`)}
	for _, n := range []string{"map", "nodes", "edges"} {
		f, _ := os.Open("../storemap/testdata/0001-" + n + ".json.gz")
		z, _ := gzip.NewReader(f)
		files["/api/0001/0001-0001-"+n+".json"], _ = io.ReadAll(z)
		f.Close()
	}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // long enough for every caller to arrive
		w.Write(files[r.URL.Path])
	}))
	defer srv.Close()
	api := bunnings.New(4)
	api.MapBaseURL, api.MapCacheDir = srv.URL, t.TempDir()
	d := &Deps{API: api}

	got := make([]*loadedMap, 8)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lm, err := d.storeMap(context.Background(), "0001")
			if err != nil {
				t.Error(err)
			}
			got[i] = lm
		}()
	}
	wg.Wait()
	for path, n := range hits {
		if n != 1 {
			t.Errorf("%s fetched %d times", path, n)
		}
	}
	if len(hits) != 4 || got[0] == nil || got[0].Graph == nil || slices.ContainsFunc(got, func(lm *loadedMap) bool { return lm != got[0] }) {
		t.Errorf("fetched %v; loads %v", hits, got)
	}
}
