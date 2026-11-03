package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/kensantoso/snag-mcp/internal/storemap"
)

// loadedMap is a store's parsed floor plan. Graph is nil when the store has
// no usable walking graph, and GraphErr then says why if it has one.
type loadedMap struct {
	Map      *storemap.Map
	Graph    *storemap.Graph
	GraphErr error
}

// storeMap loads a store's floor plan, parsed, from memory, the disk cache or
// Bunnings, in that order.
func (d *Deps) storeMap(ctx context.Context, store string) (*loadedMap, error) {
	d.mu.Lock()
	if c, ok := d.maps[store]; ok && time.Since(c.at) < cacheTTL {
		d.mu.Unlock()
		return c.v, nil
	}
	d.mu.Unlock()
	v, err, _ := d.mapLoads.Do(store, func() (any, error) {
		// Not the caller's context: a load others are waiting on must not
		// end because the first caller gave up.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		f, err := d.API.StoreMap(ctx, store)
		if err != nil {
			return nil, err
		}
		m, err := storemap.ParseMap(f.Map)
		if err != nil {
			return nil, err
		}
		lm := &loadedMap{Map: m, GraphErr: f.GraphErr}
		if f.Nodes != nil {
			// A graph that does not parse leaves straight-line ordering.
			lm.Graph, lm.GraphErr = storemap.ParseGraph(f.Nodes, f.Edges)
		}
		d.mu.Lock()
		if d.maps == nil {
			d.maps = map[string]cached[*loadedMap]{}
		}
		d.maps[store] = cached[*loadedMap]{lm, time.Now()}
		d.mu.Unlock()
		return lm, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*loadedMap), nil
}

// storeMapOut is the walk in numbers, so a client can draw its own map.
type storeMapOut struct {
	MapID string `json:"map_id"`
	// Ordering is walking_graph (walking distance over the store's own
	// wayfinding graph) or straight_line (distance between bays, when the
	// store has no graph).
	Ordering    string      `json:"ordering"`
	Coordinates string      `json:"coordinates"`
	Start       *mapPlace   `json:"start,omitempty"`
	End         *mapPlace   `json:"end,omitempty"`
	WalkM       *float64    `json:"walk_m,omitempty"`
	Storeys     []mapStorey `json:"storeys"`
	Stops       []mapStop   `json:"stops"`
	NotOnMap    []notOnMap  `json:"not_on_map,omitempty"`
	Image       bool        `json:"image_attached"`
}

type mapPlace struct {
	Name   string  `json:"name"`
	Storey int     `json:"storey"`
	X      float64 `json:"x_m"`
	Y      float64 `json:"y_m"`
}

type mapStorey struct {
	Number  int          `json:"number"`
	Name    string       `json:"name"`
	WidthM  float64      `json:"width_m"`
	HeightM float64      `json:"height_m"`
	Outline [][2]float64 `json:"outline"`
}

type mapStop struct {
	Stop    int          `json:"stop"`
	Address string       `json:"address"` // as the store's own map writes it, "022.002"
	Aisle   int          `json:"aisle"`
	Bay     int          `json:"bay"`
	Storey  int          `json:"storey"`
	X       float64      `json:"x_m"`
	Y       float64      `json:"y_m"`
	Bay2D   [][2]float64 `json:"bay_outline"`
	Items   []string     `json:"item_numbers"`
	// LegM is the walk from the previous stop, or from the entrance for the
	// first; absent when ordered by straight line.
	LegM *float64 `json:"walk_m_from_previous,omitempty"`
}

type notOnMap struct {
	ItemNumber string `json:"item_number"`
	Name       string `json:"name,omitempty"`
	// Reason is a machine name: the line's status when it is not something
	// to pick up (special_order, out_of_stock, not_sold_here, ...),
	// no_location when the store gave no aisle and bay, or
	// location_not_on_map when the store's map has no such bay.
	Reason string `json:"reason"`
}

// walkLines plans a walk through the store for the lines there is something
// to pick up, numbers those lines in walking order, and sorts the list to
// match: stops first, in order, then everything else as it was.
func walkLines(lines []shop.Line, lm *loadedMap) (storemap.Walk, []notOnMap) {
	var bays []storemap.Located
	stopOf := make([]int, len(lines)) // index into bays, plus one
	var off []notOnMap
	for i, l := range lines {
		reason := ""
		switch {
		case !l.Sold(), l.Status == shop.SpecialOrder, l.Status == shop.OutOfStock:
			reason = l.Status
		case l.Aisle == "":
			reason = "no_location"
		}
		if reason == "" {
			a, ok := parseAddr(l.Aisle, l.Bay)
			loc, found := lm.Map.Find(a)
			if !ok || !found {
				reason = "location_not_on_map"
			} else {
				k := slices.IndexFunc(bays, func(b storemap.Located) bool { return b.Bay.Addr == a })
				if k < 0 {
					k = len(bays)
					bays = append(bays, loc)
				}
				stopOf[i] = k + 1
			}
		}
		if reason != "" {
			off = append(off, notOnMap{l.ItemNumber, l.Name, reason})
		}
	}
	w := storemap.Plan(lm.Map, lm.Graph, bays)
	number := map[storemap.Addr]int{}
	for n, s := range w.Stops {
		number[s.Bay.Addr] = n + 1
	}
	for i := range lines {
		if stopOf[i] > 0 {
			lines[i].Stop = number[bays[stopOf[i]-1].Bay.Addr]
		}
	}
	slices.SortStableFunc(lines, func(a, b shop.Line) int {
		if (a.Stop == 0) != (b.Stop == 0) {
			if a.Stop == 0 {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.Stop, b.Stop)
	})
	return w, off
}

// parseAddr joins the item-location API's aisle and bay strings ("22", "2")
// to the map's integers.
func parseAddr(aisle, bay string) (storemap.Addr, bool) {
	a, err1 := strconv.Atoi(strings.TrimSpace(aisle))
	b, err2 := strconv.Atoi(strings.TrimSpace(bay))
	if err1 != nil || err2 != nil || a <= 0 || b <= 0 {
		return storemap.Addr{}, false
	}
	return storemap.Addr{Aisle: a, Bay: b}, true
}

// mapOut describes the walk in metres from each storey's top-left corner.
func mapOut(lm *loadedMap, w storemap.Walk, lines []shop.Line, off []notOnMap) *storeMapOut {
	m := lm.Map
	conv := func(s *storemap.Storey, p storemap.Point) (float64, float64) {
		return round1((p.X - s.Min.X) / 1000), round1((p.Y - s.Min.Y) / 1000)
	}
	poly := func(s *storemap.Storey, ps []storemap.Point) [][2]float64 {
		out := make([][2]float64, len(ps))
		for i, p := range ps {
			x, y := conv(s, p)
			out[i] = [2]float64{x, y}
		}
		return out
	}
	out := &storeMapOut{
		MapID: m.ID, Ordering: w.Method, NotOnMap: off, Stops: []mapStop{},
		Coordinates: "Metres from the top-left corner of each storey's outline: x across, y down, as on Bunnings' printed store maps.",
	}
	if w.Method == storemap.ByWalkingGraph {
		t := round1(w.WalkM)
		out.WalkM = &t
	}
	if w.Start != nil {
		s := m.Storey(w.StartStorey)
		x, y := conv(s, w.Start.At)
		out.Start = &mapPlace{w.Start.Name, s.Number, x, y}
		if w.End != nil {
			x, y := conv(s, w.End.At)
			out.End = &mapPlace{w.End.Name, s.Number, x, y}
		}
	}
	for _, s := range m.Storeys {
		out.Storeys = append(out.Storeys, mapStorey{
			Number: s.Number, Name: s.Name,
			WidthM: round1((s.Max.X - s.Min.X) / 1000), HeightM: round1((s.Max.Y - s.Min.Y) / 1000),
			Outline: poly(s, s.Outline),
		})
	}
	for n, st := range w.Stops {
		x, y := conv(st.Storey, st.Centre())
		ms := mapStop{
			Stop: n + 1, Address: st.Bay.Addr.String(), Aisle: st.Bay.Aisle, Bay: st.Bay.Bay,
			Storey: st.Storey.Number, X: x, Y: y, Bay2D: poly(st.Storey, st.Bay.Poly), Items: []string{},
		}
		if w.Method == storemap.ByWalkingGraph {
			leg := round1(st.LegM)
			ms.LegM = &leg
		}
		for _, l := range lines {
			if l.Stop == n+1 {
				ms.Items = append(ms.Items, l.ItemNumber)
			}
		}
		out.Stops = append(out.Stops, ms)
	}
	return out
}

// drawMap renders the walk with a caption per stop and a key underneath.
// straightLineWhy says why a walk is ordered by straight lines, not walking
// distance; "" when it is not.
func straightLineWhy(lm *loadedMap, w storemap.Walk) string {
	switch {
	case w.Method != storemap.ByStraightLine:
		return ""
	case lm.GraphErr != nil:
		return fmt.Sprintf("the store's walking graph could not be read (%v)", lm.GraphErr)
	case lm.Graph == nil:
		return "Bunnings publishes no walking graph for this store"
	case w.Start == nil:
		return "the store's plan marks no entrance"
	}
	return "a stop could not be reached on the store's walking graph"
}

func drawMap(lm *loadedMap, w storemap.Walk, st shop.Store, lines []shop.Line, off []notOnMap, checked string) ([]byte, error) {
	m := lm.Map
	d := storemap.Drawing{
		Title:  fmt.Sprintf("Store map, pick list in walking order: %s (%s)", st.Name, st.ID),
		Footer: fmt.Sprintf("Floor plan: Bunnings store map %s. Stock checked %s. Unofficial; not affiliated with Bunnings.", m.ID, checked),
	}
	switch {
	case w.Start != nil && w.Method == storemap.ByWalkingGraph:
		d.Subtitle = fmt.Sprintf("From %s", w.Start.Name)
		if w.End != nil {
			d.Subtitle += " to the " + strings.ToLower(w.End.Name)
		}
		d.Subtitle += fmt.Sprintf(", about %.0f m on foot.", w.WalkM)
	case w.Start != nil:
		d.Subtitle = fmt.Sprintf("From %s, ordered by straight-line distance between bays: %s.", w.Start.Name, straightLineWhy(lm, w))
	default:
		d.Subtitle = "Ordered by straight-line distance between bays (the map marks no entrance)."
	}
	for n, s := range w.Stops {
		var here []shop.Line
		for _, l := range lines {
			if l.Stop == n+1 {
				here = append(here, l)
			}
		}
		label := fmt.Sprintf("%d items", len(here))
		if len(here) == 1 {
			label = shortName(here[0].Name, 22) + fmt.Sprintf(" ×%d", here[0].Quantity)
		}
		d.Labels = append(d.Labels, label)
		for _, l := range here {
			floor := ""
			if len(m.Storeys) > 1 {
				floor = s.Storey.Name + " · "
			}
			d.Legend = append(d.Legend, storemap.LegendLine{Stop: n + 1, Text: fmt.Sprintf("%sAisle %d bay %d · %s ×%d · %s",
				floor, s.Bay.Aisle, s.Bay.Bay, nameOr(l), l.Quantity, statusText(l))})
		}
	}
	for _, o := range off {
		d.Legend = append(d.Legend, storemap.LegendLine{Text: fmt.Sprintf("Not on map (%s): %s %s",
			strings.ReplaceAll(o.Reason, "_", " "), o.ItemNumber, o.Name)})
	}
	return storemap.Render(m, w, d)
}

func nameOr(l shop.Line) string {
	if l.Name != "" {
		return l.Name
	}
	return l.ItemNumber
}

func statusText(l shop.Line) string {
	s := strings.ReplaceAll(l.Status, "_", " ")
	if l.Stock != nil {
		s += fmt.Sprintf(" (%d)", *l.Stock)
	}
	return s
}

// shortName cuts a product name at a word boundary for a map caption.
func shortName(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,-") + "…"
}

// mapProblem says why there is no map, for the note on the result.
func mapProblem(err error) string {
	if errors.Is(err, bunnings.ErrNoStoreMap) {
		return "Bunnings publishes no floor plan for this store, so there is no map; the list is in aisle and bay order."
	}
	return fmt.Sprintf("The store's floor plan could not be loaded (%v), so there is no map; the list is in aisle and bay order.", err)
}
