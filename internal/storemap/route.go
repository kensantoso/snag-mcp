package storemap

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Graph is the walking graph the map viewer routes over: a grid of points
// along the aisles, with the bays, desks and entrances attached to their
// nearest point.
type Graph struct {
	nodes []gnode
	adj   [][]gedge
	index map[gnode]int
	// named maps a lower-case bay address ("022.002"), landmark
	// ("registers_1") or "entrance:main entrance" on a storey to its node.
	named map[nameKey]int
}

type nameKey struct {
	name   string
	storey int
}

type gnode struct {
	P      Point
	Storey int
}

type gedge struct {
	to     int
	w      float64 // millimetres
	angle  float64
	storey int
}

// ParseGraph reads a store's -nodes.json and -edges.json. Both come as one
// storey ({resolution, data}) or several ({resolution, storeys: [...]}), and
// store y as positive, which the viewer negates on reading.
func ParseGraph(nodesJSON, edgesJSON []byte) (*Graph, error) {
	var e struct {
		Resolution *float64      `json:"resolution"`
		Data       [][][]float64 `json:"data"`
		Storeys    []storeyData  `json:"storeys"`
		Level      [][][]float64 `json:"levelAccesses"`
	}
	var n struct {
		Resolution *float64               `json:"resolution"`
		Data       map[string]*[2]float64 `json:"data"`
		Storeys    []struct {
			Storey int                    `json:"storey"`
			Data   map[string]*[2]float64 `json:"data"`
		} `json:"storeys"`
	}
	if err := json.Unmarshal(edgesJSON, &e); err != nil {
		return nil, fmt.Errorf("walking graph edges: %w", err)
	}
	if err := json.Unmarshal(nodesJSON, &n); err != nil {
		return nil, fmt.Errorf("walking graph nodes: %w", err)
	}
	if e.Storeys == nil {
		e.Storeys = []storeyData{{Storey: 1, Data: e.Data}}
	}
	if n.Storeys == nil {
		n.Storeys = append(n.Storeys, struct {
			Storey int                    `json:"storey"`
			Data   map[string]*[2]float64 `json:"data"`
		}{1, n.Data})
	}
	g := &Graph{index: map[gnode]int{}, named: map[nameKey]int{}}
	er, nr := resolution(e.Resolution), resolution(n.Resolution)
	for _, s := range e.Storeys {
		for _, pair := range s.Data {
			if len(pair) != 2 || len(pair[0]) < 2 || len(pair[1]) < 2 {
				continue
			}
			g.link(gnode{Point{pair[0][0] * er, -pair[0][1] * er}, s.Storey},
				gnode{Point{pair[1][0] * er, -pair[1][1] * er}, s.Storey})
		}
	}
	for _, l := range e.Level {
		// [[x, y, storey], [x, y, storey]]; anything shorter is not a link.
		if len(l) < 2 || len(l[0]) < 3 || len(l[1]) < 3 {
			continue
		}
		g.link(gnode{Point{l[0][0], -l[0][1]}, int(l[0][2])}, gnode{Point{l[1][0], -l[1][1]}, int(l[1][2])})
	}
	if len(g.nodes) == 0 {
		return nil, fmt.Errorf("walking graph: no edges")
	}
	for _, s := range n.Storeys {
		for name, p := range s.Data {
			if p == nil {
				continue
			}
			// As in the viewer, a name counts only if its point is on the graph.
			if i, ok := g.index[gnode{Point{p[0] * nr, -p[1] * nr}, s.Storey}]; ok {
				g.named[nameKey{strings.ToLower(name), s.Storey}] = i
			}
		}
	}
	return g, nil
}

type storeyData struct {
	Storey int           `json:"storey"`
	Data   [][][]float64 `json:"data"`
}

func resolution(r *float64) float64 {
	if r == nil {
		return 1000
	}
	return *r
}

func (g *Graph) node(n gnode) int {
	if i, ok := g.index[n]; ok {
		return i
	}
	g.index[n] = len(g.nodes)
	g.nodes = append(g.nodes, n)
	g.adj = append(g.adj, nil)
	return len(g.nodes) - 1
}

func (g *Graph) link(a, b gnode) {
	i, j := g.node(a), g.node(b)
	w := dist(a.P, b.P)
	ang := math.Atan2(b.P.Y-a.P.Y, b.P.X-a.P.X) * 180 / math.Pi
	g.adj[i] = append(g.adj[i], gedge{j, w, ang, b.Storey})
	g.adj[j] = append(g.adj[j], gedge{i, w, math.Mod(ang+180, 360), a.Storey})
}

// nodeFor is the graph node for a named place on the storey, else the nearest
// node on the storey to p.
func (g *Graph) nodeFor(name string, p Point, storey int) int {
	if i, ok := g.named[nameKey{strings.ToLower(name), storey}]; ok && name != "" {
		return i
	}
	best, bd := -1, math.Inf(1)
	for i, n := range g.nodes {
		if n.Storey != storey {
			continue
		}
		if d := dist(n.P, p); d < bd {
			best, bd = i, d
		}
	}
	return best
}

// shortest runs the viewer's search from src: distance-weighted, a tenth
// dearer for each turn so routes follow aisles rather than staircase across
// the grid, and thirty times dearer to change storey so a walk finishes a
// floor before taking the stairs.
func (g *Graph) shortest(src int) (cost []float64, prev []int32) {
	cost = make([]float64, len(g.nodes))
	prev = make([]int32, len(g.nodes))
	for i := range cost {
		cost[i], prev[i] = math.Inf(1), -1
	}
	cost[src] = 0
	q := &pq{{node: src, storey: g.nodes[src].Storey, angle: math.NaN()}}
	for q.Len() > 0 {
		it := heap.Pop(q).(pqItem)
		if it.cost > cost[it.node] {
			continue
		}
		for _, e := range g.adj[it.node] {
			f := 1.0
			if !math.IsNaN(it.angle) && e.angle != it.angle {
				f = 1.1
			}
			if e.storey != it.storey {
				f = 30
			}
			if c := cost[it.node] + f*e.w; c < cost[e.to] {
				cost[e.to], prev[e.to] = c, int32(it.node)
				heap.Push(q, pqItem{e.to, e.storey, e.angle, c})
			}
		}
	}
	return cost, prev
}

type pqItem struct {
	node   int
	storey int
	angle  float64
	cost   float64
}

type pq []pqItem

func (q pq) Len() int           { return len(q) }
func (q pq) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q pq) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *pq) Push(x any)        { *q = append(*q, x.(pqItem)) }
func (q *pq) Pop() any {
	old := *q
	it := old[len(old)-1]
	*q = old[:len(old)-1]
	return it
}

// Ordering methods.
const (
	// ByWalkingGraph orders stops by walking distance over the store's own
	// walking graph, the one its map viewer gives directions with.
	ByWalkingGraph = "walking_graph"
	// ByStraightLine orders stops by straight-line distance between bays,
	// used when the store has no usable walking graph. It ignores shelving
	// in the way, so it is a reasonable order but not a walking distance.
	ByStraightLine = "straight_line"
)

// Walk is a planned route through a store.
type Walk struct {
	Method      string
	Start       *Label // the entrance the walk starts from; nil if the map has none
	StartStorey int
	End         *Label // the registers it ends at; nil if the map has none
	Stops       []Stop // in walking order
	// WalkM is the walking distance in metres over the graph, start to end.
	// Zero when ordered by straight line: no walking distance is known.
	WalkM float64
	// Legs are the walking paths into each stop and, last, to the end, for
	// drawing. Empty when ordered by straight line.
	Legs [][]PathPoint
}

// PathPoint is a point on a walk; a walk between floors has points on each.
type PathPoint struct {
	Point
	Storey int
}

// Stop is one bay on the walk.
type Stop struct {
	Located
	LegM float64 // walking metres from the previous stop; 0 by straight line
}

// Plan orders the bays into a walk from the main entrance, ending at the
// registers nearest it. With a walking graph, distances are walking distances
// over it; without one, or if a bay cannot be reached on it, they are
// straight lines between bays.
func Plan(m *Map, g *Graph, bays []Located) Walk { return plan(m, g, bays, false) }

// Retrace walks the bays in the order given rather than planning one, from
// the same entrance to the same registers. Given the stops of a Plan, it
// gives back the same walk, legs included, which is how a client that has
// only the stop order gets the route to draw.
func Retrace(m *Map, g *Graph, bays []Located) Walk { return plan(m, g, bays, true) }

func plan(m *Map, g *Graph, bays []Located, fixed bool) Walk {
	w := Walk{Method: ByStraightLine}
	var startP Point
	var startName string
	if s, e, ok := m.MainEntrance(); ok {
		w.Start, w.StartStorey, startP, startName = &e, s.Number, e.At, e.Name
		if r, ok := s.Registers(e.At); ok {
			w.End = &Label{r.Name, centre(r.Poly)}
		}
	}
	if len(bays) == 0 {
		return w
	}
	if g != nil && w.Start != nil {
		if gw, ok := planGraph(g, w, startName, startP, bays, fixed); ok {
			return gw
		}
	}
	// Straight lines: a storey apart counts as 100 m, so floors stay together.
	pts := make([]Point, 0, len(bays)+2)
	floors := make([]int, 0, len(bays)+2)
	pts, floors = append(pts, startP), append(floors, w.StartStorey)
	for _, b := range bays {
		pts, floors = append(pts, b.Centre()), append(floors, b.Storey.Number)
	}
	if w.End != nil {
		pts, floors = append(pts, w.End.At), append(floors, w.StartStorey)
	}
	cost := matrix(len(pts), func(i, j int) float64 {
		if i == 0 && w.Start == nil {
			return 0 // no entrance: start wherever is best
		}
		d := dist(pts[i], pts[j])
		if floors[i] != floors[j] {
			d += 100000
		}
		return d
	})
	for _, i := range visit(cost, w.End != nil, fixed) {
		w.Stops = append(w.Stops, Stop{Located: bays[i-1]})
	}
	return w
}

func planGraph(g *Graph, w Walk, startName string, startP Point, bays []Located, fixed bool) (Walk, bool) {
	ids := []int{g.nodeFor("entrance:"+startName, startP, w.StartStorey)}
	for _, b := range bays {
		ids = append(ids, g.nodeFor(b.Bay.Addr.String(), b.Centre(), b.Storey.Number))
	}
	if w.End != nil {
		ids = append(ids, g.nodeFor("", w.End.At, w.StartStorey))
	}
	if slices.Contains(ids, -1) {
		return w, false
	}
	// One search from the start and from each bay; the graph is undirected,
	// so those give every distance the order needs, including to the end.
	runs := make([][]float64, len(ids))
	prevs := make([][]int32, len(ids))
	for i := range ids {
		if w.End != nil && i == len(ids)-1 {
			break
		}
		runs[i], prevs[i] = g.shortest(ids[i])
	}
	cost := matrix(len(ids), func(i, j int) float64 {
		if runs[i] == nil {
			return runs[j][ids[i]] // from the end, which no walk does
		}
		return runs[i][ids[j]]
	})
	for i := range ids {
		for j := 1; j < len(ids); j++ {
			if i != j && math.IsInf(cost[i][j], 1) {
				return w, false // a bay the graph cannot reach
			}
		}
	}
	w.Method = ByWalkingGraph
	prev := 0
	seq := visit(cost, w.End != nil, fixed)
	if w.End != nil {
		seq = append(seq, len(ids)-1)
	}
	for _, i := range seq {
		path := g.path(prevs[prev], ids[prev], ids[i])
		legM := length(path) / 1000
		w.WalkM += legM
		w.Legs = append(w.Legs, simplify(path))
		if i <= len(bays) {
			w.Stops = append(w.Stops, Stop{Located: bays[i-1], LegM: legM})
		}
		prev = i
	}
	return w, true
}

func (g *Graph) path(prev []int32, from, to int) []PathPoint {
	var out []PathPoint
	for n := to; n >= 0; n = int(prev[n]) {
		out = append(out, PathPoint{g.nodes[n].P, g.nodes[n].Storey})
		if n == from {
			break
		}
	}
	slices.Reverse(out)
	return out
}

// length is the distance walked on each floor; going between floors counts
// as nothing, since the lift does the work.
func length(ps []PathPoint) float64 {
	var t float64
	for i := 1; i < len(ps); i++ {
		if ps[i].Storey == ps[i-1].Storey {
			t += dist(ps[i-1].Point, ps[i].Point)
		}
	}
	return t
}

// simplify drops points in the middle of straight runs on one floor.
func simplify(ps []PathPoint) []PathPoint {
	if len(ps) < 3 {
		return ps
	}
	out := []PathPoint{ps[0]}
	for i := 1; i < len(ps)-1; i++ {
		a, b, c := out[len(out)-1], ps[i], ps[i+1]
		if a.Storey != b.Storey || b.Storey != c.Storey || (b.X-a.X)*(c.Y-b.Y)-(b.Y-a.Y)*(c.X-b.X) != 0 {
			out = append(out, b)
		}
	}
	return append(out, ps[len(ps)-1])
}

// visit is the order to visit the bays in: the cheapest, or as given.
func visit(cost [][]float64, hasEnd, fixed bool) []int {
	if !fixed {
		return order(cost, hasEnd)
	}
	k := len(cost) - 1
	if hasEnd {
		k--
	}
	seq := make([]int, k)
	for i := range seq {
		seq[i] = i + 1
	}
	return seq
}

func matrix(n int, f func(i, j int) float64) [][]float64 {
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
		for j := range m[i] {
			if i != j {
				m[i][j] = f(i, j)
			}
		}
	}
	return m
}

// order finds the cheapest order to visit points 1..k starting from point 0
// and, when hasEnd, finishing at point k+1. It is exact up to 12 stops and a
// nearest-neighbour tour improved by 2-opt beyond that.
func order(cost [][]float64, hasEnd bool) []int {
	k := len(cost) - 1
	end := -1
	if hasEnd {
		k--
		end = k + 1
	}
	tail := func(j int) float64 {
		if end < 0 {
			return 0
		}
		return cost[j][end]
	}
	if k <= 12 {
		return heldKarp(cost, k, tail)
	}
	// Nearest neighbour, then reverse any stretch that shortens the walk.
	seq, used, at := make([]int, 0, k), make([]bool, k+1), 0
	for len(seq) < k {
		best := -1
		for j := 1; j <= k; j++ {
			if !used[j] && (best < 0 || cost[at][j] < cost[at][best]) {
				best = j
			}
		}
		used[best], at = true, best
		seq = append(seq, best)
	}
	total := func(s []int) float64 {
		t, p := 0.0, 0
		for _, j := range s {
			t, p = t+cost[p][j], j
		}
		return t + tail(p)
	}
	for improved := true; improved; {
		improved = false
		for i := 0; i < k-1; i++ {
			for j := i + 1; j < k; j++ {
				c := slices.Clone(seq)
				slices.Reverse(c[i : j+1])
				if total(c) < total(seq)-1e-6 {
					seq, improved = c, true
				}
			}
		}
	}
	return seq
}

func heldKarp(cost [][]float64, k int, tail func(int) float64) []int {
	if k == 0 {
		return nil
	}
	full := 1<<k - 1
	dp := make([][]float64, full+1)
	par := make([][]int8, full+1)
	for m := range dp {
		dp[m] = make([]float64, k)
		par[m] = make([]int8, k)
		for j := range dp[m] {
			dp[m][j] = math.Inf(1)
		}
	}
	for j := 0; j < k; j++ {
		dp[1<<j][j] = cost[0][j+1]
		par[1<<j][j] = -1
	}
	for m := 1; m <= full; m++ {
		for j := 0; j < k; j++ {
			if m&(1<<j) == 0 || math.IsInf(dp[m][j], 1) {
				continue
			}
			for n := 0; n < k; n++ {
				if m&(1<<n) != 0 {
					continue
				}
				if c := dp[m][j] + cost[j+1][n+1]; c < dp[m|1<<n][n] {
					dp[m|1<<n][n], par[m|1<<n][n] = c, int8(j)
				}
			}
		}
	}
	last, best := 0, math.Inf(1)
	for j := 0; j < k; j++ {
		if c := dp[full][j] + tail(j+1); c < best {
			last, best = j, c
		}
	}
	seq := make([]int, 0, k)
	for m, j := full, last; j >= 0; {
		seq = append(seq, j+1)
		pj := int(par[m][j])
		m, j = m&^(1<<j), pj
	}
	slices.Reverse(seq)
	return seq
}

// FloorChange says how the walk reaches stop i from the stop before it (the
// entrance for the first), or, with i == len(Stops), reaches the end: the
// storey it arrives on and what it takes there, "lift", "travelator" or
// "stairs" as the map names it, or "" when the way is not known. to is nil
// when the walk stays on one floor.
func (w Walk) FloorChange(m *Map, i int) (to *Storey, via string) {
	from := w.StartStorey
	if i > 0 && i <= len(w.Stops) {
		from = w.Stops[i-1].Storey.Number
	}
	dest := m.Storey(w.StartStorey)
	if i < len(w.Stops) {
		dest = w.Stops[i].Storey
	}
	if dest == nil || dest.Number == from {
		return nil, ""
	}
	if i >= len(w.Legs) {
		return dest, ""
	}
	// Where the path first leaves the floor: the level access at each end,
	// which the map may name differently (a travelator on one floor
	// arrives at a lift below).
	leg := w.Legs[i]
	for k := 1; k < len(leg); k++ {
		if leg[k].Storey == leg[k-1].Storey {
			continue
		}
		var names []string
		for _, p := range []PathPoint{leg[k-1], leg[k]} {
			s := m.Storey(p.Storey)
			if s == nil {
				continue
			}
			best, bd := "", math.Inf(1)
			for _, a := range s.LevelAccess {
				if d := dist(centre(a.Poly), p.Point); d < bd {
					best, bd = strings.ToLower(a.Name), d
				}
			}
			if best != "" && !slices.Contains(names, best) {
				names = append(names, best)
			}
		}
		return dest, strings.Join(names, " or ")
	}
	return dest, ""
}
