package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/kensantoso/snag-mcp/internal/storemap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxListItems is the most different items the multi-store stock lookup
// takes in one request.
const maxListItems = 30

type listItem struct {
	ItemNumber string `json:"item_number" jsonschema:"Bunnings item number from search_products"`
	Quantity   int    `json:"quantity,omitempty" jsonschema:"How many. Default 1."`
}

type checkListIn struct {
	Items    []listItem `json:"items" jsonschema:"The shopping list: item numbers from search_products with quantities"`
	Store    string     `json:"store,omitempty" jsonschema:"Store id (e.g. '6400') or name (e.g. 'Mentone') for an aisle-sorted pick list there. Omit to rank every nearby store by how much of the list it can fill."`
	Location string     `json:"location,omitempty" jsonschema:"Postcode or suburb for this call only. Omit to use the saved location."`
	RadiusKM float64    `json:"radius_km,omitempty" jsonschema:"How far to look for stores, in km. Default 50."`
	Map      *bool      `json:"map,omitempty" jsonschema:"With store: whether to order the pick list by a walk through the store and attach a floor plan image with each stop numbered. Default true; false gives plain aisle and bay order with no image."`
}

type pickList struct {
	Store shop.Store  `json:"store"`
	Lines []shop.Line `json:"lines"`
	// Total is price times quantity over every line sold at this store,
	// whether or not it is all in stock.
	Total      float64 `json:"total"`
	AllInStock bool    `json:"all_in_stock"`
}

type checkListOut struct {
	PickList *pickList     `json:"pick_list,omitempty"`
	StoreMap *storeMapOut  `json:"store_map,omitempty"`
	MapNote  string        `json:"map_note,omitempty"`
	Stores   []shop.Ranked `json:"stores,omitempty"`
	Load     shop.Load     `json:"load"`
	Coverage Coverage      `json:"coverage"`
	Checked  string        `json:"stock_checked_at"`
	Note     string        `json:"note,omitempty"`
}

func registerCheckList(s *mcp.Server, d *Deps) {
	addTool(s, &mcp.Tool{
		Name: "check_list",
		Description: "Check a whole shopping list against live stock. " +
			"Without `store`: ranks the nearby stores by how many lines each can fill from stock right now (then by distance), " +
			"with each store's cost for those lines and what is missing, so you can say where to go for one trip. " +
			"With `store`: a pick list for that store with stock, price and line totals, and a grand total, in walking order: " +
			"each line to pick up gets a `stop` number along a walk from the main entrance to the registers over the store's own floor plan, " +
			"and a PNG floor plan is attached with those bays highlighted and numbered to match. Read the stops to the user in that order. " +
			"Lines with nothing to pick up there (special order, out of stock, not sold, no aisle) are listed as not on the map. " +
			"`store_map` has the stop coordinates. A store with no published plan, or `map: false`, gives aisle then bay order instead. " +
			"Line status is a machine name: in_stock, short (fewer than wanted), out_of_stock, special_order, stock_unknown, " +
			"not_sold_here, marketplace, inactive, not_found. Stock is an exact count at the time checked. " +
			"Both include a load summary from published figures only: the longest item, total known weight, and items whose " +
			"weight or size is unknown (never estimate these; say so). Rough vehicle rules of thumb to advise from, stated as approximate: " +
			"up to ~1.2 m fits most hatches and sedans with seats folded; ~1.2-2.0 m needs a wagon or SUV with seats down; " +
			"over ~2.4 m (most timber, 2400x1200 sheets) needs a ute, roof racks or a trailer; " +
			"heavy loads (several hundred kg, e.g. bagged cement) can exceed a car's payload, so suggest splitting trips or delivery.",
		// Hosts that render MCP Apps show the pick list and map inline.
		Meta: uiMeta(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in checkListIn) (*mcp.CallToolResult, checkListOut, error) {
		wants, err := normaliseList(in.Items)
		if err != nil {
			return nil, checkListOut{}, err
		}
		a, err := d.nearby(ctx, req, in.Location, in.RadiusKM)
		if err != nil {
			return nil, checkListOut{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()

		out := checkListOut{Coverage: a.cov, Checked: time.Now().Format("2006-01-02 15:04 MST")}
		items := make([]string, len(wants))
		for i, w := range wants {
			items[i] = w.ItemNumber
		}

		var stores []shop.Store
		if in.Store != "" {
			st, err := d.findStore(ctx, a, in.Store)
			if err != nil {
				return nil, checkListOut{}, err
			}
			stores = []shop.Store{st}
		} else {
			if len(a.stores) == 0 {
				out.Note = a.cov.Note
				return textAndJSON(a.cov.Note, out), out, nil
			}
			stores = a.searchStores()
		}

		raws, err := d.itemsAt(ctx, items, stores, a.region)
		if err != nil {
			return nil, checkListOut{}, err
		}
		products := d.productsFor(ctx, wants, stores[0].ID)
		if in.Store != "" {
			st := stores[0]
			out.Coverage.StoresCovered = 1
			// The floor plan downloads while stock is checked.
			type mapResult struct {
				lm  *loadedMap
				err error
			}
			mapCh := make(chan mapResult, 1)
			wantMap := in.Map == nil || *in.Map
			if wantMap {
				go func() {
					lm, err := d.storeMap(ctx, st.ID)
					mapCh <- mapResult{lm, err}
				}()
			}
			lines := d.storeLines(ctx, st, wants, raws)
			shop.SortPickList(lines)
			var img []byte
			var walk *storemap.Walk
			var plan *storemap.Map
			if wantMap {
				switch r := <-mapCh; {
				case r.err != nil:
					out.MapNote = mapProblem(r.err)
				default:
					w, off := walkLines(lines, r.lm)
					if len(w.Stops) == 0 {
						out.MapNote = "Nothing on this list is to be picked up from a bay on the store's floor plan, so there is no map."
						break
					}
					walk, plan = &w, r.lm.Map
					if why := straightLineWhy(r.lm, w); why != "" {
						out.MapNote = "Stops are ordered by straight-line distance between bays, not walking distance: " + why + "."
					}
					out.StoreMap = mapOut(r.lm, w, lines, off)
					var err error
					if img, err = drawMap(r.lm, w, st, lines, off, out.Checked); err != nil {
						out.MapNote = fmt.Sprintf("The walking order is set but the floor plan could not be drawn: %v", err)
					}
					out.StoreMap.Image = img != nil
				}
			}
			pl := &pickList{Store: st, Lines: lines, Total: shop.Total(lines), AllInStock: true}
			for _, l := range lines {
				pl.AllInStock = pl.AllInStock && l.Status == shop.InStock
			}
			out.PickList = pl
			out.Load = load(soldSomewhere(wants, raws, stores), raws, products)
			res := textAndJSON(pickSummary(pl, out.Load, walk, plan, out.MapNote, img != nil), out)
			if img != nil {
				// After the summary, before the JSON: clients that show
				// images show it with the text that explains it.
				res.Content = slices.Insert(res.Content, 1, mcp.Content(&mcp.ImageContent{Data: img, MIMEType: "image/png"}))
			}
			return res, out, nil
		}

		stock, err := d.API.StockNear(ctx, a.place.Lat, a.place.Lon, a.radius+5, items)
		if err != nil {
			return nil, checkListOut{}, fmt.Errorf("could not read stock across stores (%v); pass `store` to check one store at a time", err)
		}
		lines := make(map[string][]shop.Line, len(stores))
		var covered []shop.Store
		for _, st := range stores {
			at, ok := stock[st.ID]
			if !ok {
				continue // outside the gateway's radius; no stock to report
			}
			covered = append(covered, st)
			ls := make([]shop.Line, len(wants))
			for i, w := range wants {
				ss, found := at[w.ItemNumber]
				p := products[w.ItemNumber]
				c := shop.Check{Raw: raws[w.ItemNumber], Offer: offerAt(raws[w.ItemNumber], st)}.
					FromStockNear(found, ss, p != nil && p.IsSpecial)
				ls[i] = shop.MakeLine(w.ItemNumber, w.Quantity, c)
			}
			lines[st.ID] = ls
		}
		out.Coverage.StoresCovered = len(covered)
		if len(covered) < len(a.stores) {
			out.Coverage.Note = fmt.Sprintf("Ranked %d of the %d stores within %.0f km; the rest returned no stock or exceed the %d-store search limit.",
				len(covered), len(a.stores), a.radius, maxSearchStores)
		}
		out.Stores = shop.Rank(covered, lines)
		out.Load = load(soldSomewhere(wants, raws, covered), raws, products)
		return textAndJSON(rankSummary(out.Stores, len(wants), out.Coverage, out.Load), out), out, nil
	})
}

func offerAt(r bunnings.Raw, st shop.Store) *shop.Offer {
	if os := shop.Offers(r, []shop.Store{st}); len(os) == 1 {
		return &os[0]
	}
	return nil
}

func normaliseList(items []listItem) ([]listItem, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("items is empty: pass item numbers from search_products with quantities")
	}
	var out []listItem
	idx := map[string]int{}
	for _, it := range items {
		n := strings.TrimSpace(it.ItemNumber)
		if !bunnings.ValidItemNumber(n) {
			return nil, fmt.Errorf("%q is not an item number; use the item_number from search_products", it.ItemNumber)
		}
		q := it.Quantity
		if q <= 0 {
			q = 1
		}
		// The same item listed twice is one line with the quantities added.
		if i, ok := idx[n]; ok {
			out[i].Quantity += q
			continue
		}
		idx[n] = len(out)
		out = append(out, listItem{n, q})
	}
	if len(out) > maxListItems {
		return nil, fmt.Errorf("at most %d different items per list", maxListItems)
	}
	return out, nil
}

// findStore resolves a store id or name, first among stores within the radius
// and then a little further out, since the user may name a store they know.
func (d *Deps) findStore(ctx context.Context, a *area, want string) (shop.Store, error) {
	match := func(list []shop.Store) (shop.Store, bool) {
		w := strings.ToLower(strings.TrimSpace(want))
		for _, s := range list {
			if s.ID == w || strings.ToLower(s.Name) == w {
				return s, true
			}
		}
		for _, s := range list {
			if strings.Contains(strings.ToLower(s.Name), w) {
				return s, true
			}
		}
		return shop.Store{}, false
	}
	if s, ok := match(a.stores); ok {
		return s, nil
	}
	wide, err := d.around(ctx, a.place, maxRadiusKM)
	if err != nil {
		return shop.Store{}, err
	}
	if s, ok := match(wide.stores); ok {
		return s, nil
	}
	return shop.Store{}, fmt.Errorf("no store %q within %d km of %s; use a store_id or name from search_products", want, maxRadiusKM, a.place.Label())
}

// itemsAt fetches the search hits for a list with fields for the given stores,
// in one request. Missing items are absent from the map.
func (d *Deps) itemsAt(ctx context.Context, items []string, stores []shop.Store, region string) (map[string]bunnings.Raw, error) {
	res, err := d.API.Search(ctx, bunnings.SearchRequest{
		AQ: shop.ItemsQuery(items), Fields: shop.SearchFields(stores, region),
		Store: stores[0].ID, Region: region, Count: len(items) + 5,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]bunnings.Raw{}
	for _, r := range res.Results {
		out[r.Str("itemnumber")] = r
	}
	return out, nil
}

// storeLines checks each list item at one store. Stock is looked up only for
// items that qualify there; a store that does not sell an item needs no call
// to say so. A failed lookup leaves that line's stock unknown rather than
// failing the list.
func (d *Deps) storeLines(ctx context.Context, st shop.Store, wants []listItem, raws map[string]bunnings.Raw) []shop.Line {
	lines := make([]shop.Line, len(wants))
	var wg sync.WaitGroup
	for i, w := range wants {
		r := raws[w.ItemNumber]
		offer := offerAt(r, st)
		if offer == nil {
			lines[i] = shop.MakeLine(w.ItemNumber, w.Quantity, shop.Check{Raw: r})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := shop.Check{Raw: r, Offer: offer}
			if f, err := d.API.Fulfilment(ctx, w.ItemNumber, st.ID); err == nil {
				c = c.FromFulfilment(f)
			}
			c.Locations, _ = d.API.Locations(ctx, w.ItemNumber, st.ID)
			lines[i] = shop.MakeLine(w.ItemNumber, w.Quantity, c)
		}()
	}
	wg.Wait()
	return lines
}

// soldSomewhere keeps the list items at least one of the stores sells, which
// are the only ones that could end up in the car.
func soldSomewhere(wants []listItem, raws map[string]bunnings.Raw, stores []shop.Store) []listItem {
	var out []listItem
	for _, w := range wants {
		if len(shop.Offers(raws[w.ItemNumber], stores)) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// productsFor fetches product details for a list, concurrently. A product whose
// details cannot be fetched is absent, and counts as unknown size and weight.
func (d *Deps) productsFor(ctx context.Context, wants []listItem, store string) map[string]*bunnings.Product {
	out := map[string]*bunnings.Product{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, w := range wants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := d.product(ctx, w.ItemNumber, store); err == nil {
				mu.Lock()
				out[w.ItemNumber] = p
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

func load(wants []listItem, raws map[string]bunnings.Raw, products map[string]*bunnings.Product) shop.Load {
	items := make([]shop.LoadItem, len(wants))
	for i, w := range wants {
		items[i] = shop.LoadItem{Item: w.ItemNumber, Qty: w.Quantity}
		if r := raws[w.ItemNumber]; r != nil {
			items[i].Name = strings.TrimSpace(r.Str("name"))
		}
		if p := products[w.ItemNumber]; p != nil {
			if items[i].Name == "" {
				items[i].Name = strings.TrimSpace(p.Name)
			}
			items[i].WeightKG = shop.ParseWeightKG(p.Weight)
			items[i].Packages = packagesOf(p)
		}
	}
	return shop.Summarise(items)
}

// pickSummary is the pick list as text. In a store on more than one floor,
// each stop names its floor and the walk says where it changes floor, since
// the text is all a client without images has.
func pickSummary(pl *pickList, l shop.Load, w *storemap.Walk, m *storemap.Map, mapNote string, image bool) string {
	floors := w != nil && m != nil && len(m.Storeys) > 1
	var b strings.Builder
	fmt.Fprintf(&b, "Pick list for %s (%.1f km). Total $%.2f.", pl.Store.Name, pl.Store.DistanceKM, pl.Total)
	if !pl.AllInStock {
		b.WriteString(" Not everything is in stock.")
	}
	switch {
	case w != nil && w.Start != nil:
		fmt.Fprintf(&b, " Walking order from %s", w.Start.Name)
		if s := m.Storey(w.StartStorey); floors && s != nil {
			fmt.Fprintf(&b, " (%s)", s.Name)
		}
		if w.End != nil {
			b.WriteString(" to the " + strings.ToLower(w.End.Name))
		}
		if w.Method == storemap.ByWalkingGraph {
			fmt.Fprintf(&b, ", about %.0f m on foot over the store's floor plan.", w.WalkM)
		} else {
			b.WriteString(", by straight-line distance between bays.")
		}
	case w != nil:
		b.WriteString(" Walking order by straight-line distance between bays.")
	default:
		b.WriteString(" Aisle then bay order.")
	}
	if image {
		b.WriteString(" A floor plan with the stops numbered is attached.")
	}
	if mapNote != "" {
		b.WriteString(" " + mapNote)
	}
	change := func(i int, format string) {
		if to, via := w.FloorChange(m, i); floors && to != nil {
			if via == "" {
				via = "stairs or lift"
			}
			fmt.Fprintf(&b, "\n"+format, via, to.Name)
		}
	}
	finished := w == nil
	finish := func() {
		if !finished && w.End != nil {
			change(len(w.Stops), "Then take the %s back to %s for the registers.")
		}
		finished = true
	}
	last := 0
	for _, x := range pl.Lines {
		if x.Stop == 0 {
			finish()
		} else if x.Stop != last {
			change(x.Stop-1, "Take the %s to %s.")
			last = x.Stop
		}
		loc := "no location"
		if x.Aisle != "" {
			loc = "aisle " + x.Aisle + " bay " + x.Bay
		}
		switch {
		case x.Stop > 0 && floors:
			fmt.Fprintf(&b, "\n%d. %s: %s x%d %s: %s", x.Stop, w.Stops[x.Stop-1].Storey.Name, x.ItemNumber, x.Quantity, x.Name, x.Status)
		case x.Stop > 0:
			fmt.Fprintf(&b, "\n%d. %s x%d %s: %s", x.Stop, x.ItemNumber, x.Quantity, x.Name, x.Status)
		case w != nil:
			fmt.Fprintf(&b, "\n- (not on map) %s x%d %s: %s", x.ItemNumber, x.Quantity, x.Name, x.Status)
		default:
			fmt.Fprintf(&b, "\n- %s x%d %s: %s", x.ItemNumber, x.Quantity, x.Name, x.Status)
		}
		if x.Sold() {
			fmt.Fprintf(&b, ", %s, $%.2f", loc, *x.LineTotal)
		}
		if x.Stock != nil {
			fmt.Fprintf(&b, ", stock %d", *x.Stock)
		}
	}
	finish()
	b.WriteString("\n" + loadLine(l))
	return b.String()
}

func rankSummary(rs []shop.Ranked, n int, cov Coverage, l shop.Load) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Checked stock for %s at %d of %s within %.0f km of %s.", plural(n, "item"), cov.StoresCovered, plural(cov.StoresInRadius, "store"), cov.RadiusKM, cov.Location)
	for _, r := range rs[:min(5, len(rs))] {
		fmt.Fprintf(&b, "\n- %s (%.1f km): %d/%d lines in stock, $%.2f", r.Name, r.DistanceKM, r.Fillable, r.Of, r.CostInStock)
		if len(r.Missing) > 0 {
			var m []string
			for _, x := range r.Missing {
				m = append(m, x.ItemNumber+" "+x.Status)
			}
			fmt.Fprintf(&b, "; missing %s", strings.Join(m, ", "))
		}
	}
	if !slices.ContainsFunc(rs, func(r shop.Ranked) bool { return r.Complete }) {
		b.WriteString("\nNo checked store can fill the whole list from stock.")
	}
	b.WriteString("\n" + loadLine(l))
	return b.String()
}

func loadLine(l shop.Load) string {
	s := fmt.Sprintf("Load: known weight %.1f kg", l.KnownWeightKG)
	if l.Longest != nil {
		s += fmt.Sprintf(", longest %.2f m (%s)", l.Longest.LengthM, l.Longest.Name)
	}
	if len(l.WeightUnknown) > 0 {
		s += fmt.Sprintf("; weight unknown for %d item(s)", len(l.WeightUnknown))
	}
	return s + "."
}
