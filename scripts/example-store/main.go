// Command example-store writes Example Store (0001), the invented two-floor
// store the offline tests and the README's pictures use, in the format of the
// map viewer's -map.json, -nodes.json and -edges.json:
//
//	go run ./scripts/example-store internal/storemap/testdata
//
// With -demo, it instead runs the real tools against an invented website that
// serves this store and a made-up shopping list, calls check_list for it as an
// MCP Apps host would, and writes what came back: the pick list's text, its
// PNG, and a recording scripts/app-preview/preview.py can replay with
// --record:
//
//	go run ./scripts/example-store -demo /tmp/demo
//
// Nothing in it comes from a real store. It has what the code must handle in
// the real ones: two storeys joined by a travelator and a lift, the main
// entrance and registers upstairs, aisles across one floor and up and down
// the other, numbered and unnumbered shelving, promotional displays (one
// sharing a racking bay's number), landmarks and departments with the
// viewer's "_N" suffixes, and a walking grid with y stored negated.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

const (
	store  = "0001"
	mapID  = "0001-0001"
	name   = "Example Store"
	grid   = 1000 // walking grid spacing, mm
	clear  = 300  // how far the grid keeps from shelving and walls, mm
	bayW   = 2700 // a bay's length along its aisle
	bayD   = 600  // a bay's depth
	aisleW = 4000 // centre to centre
)

type pt struct{ X, Y int }

type rect struct{ x0, y0, x1, y1 int }

func (r rect) poly() []pt {
	return []pt{{r.x0, r.y0}, {r.x0, r.y1}, {r.x1, r.y1}, {r.x1, r.y0}, {r.x0, r.y0}}
}

type bay struct {
	aisle, n int // 0, 0: shelving the map does not number
	r        rect
	face     pt // unit step from the bay towards the aisle it faces
	promo    bool
}

type named struct {
	name string
	at   pt
}

type area struct {
	name string
	r    rect
}

type floor struct {
	number      int
	name, abbr  string
	outline     []pt
	aisles      []named
	bays        []bay
	landmarks   []area
	departments []named
	entrances   []named
	level       []area
}

// across adds a block of aisles running left to right from x0, ten bays
// long: aisle first at y0 and each next one step further down the page (a
// negative step goes up), with ten bays a side numbered from the right-hand
// end, 1-10 on the side above the aisle and 11-20 below. The outermost sides
// are wall racking. top adds unnumbered shelving on the far side of the rack
// above the topmost aisle.
func (f *floor) across(first, count, x0, y0, step int, top bool) {
	const n = 10
	for k := range count {
		a, y := first+k, y0+k*step
		f.aisles = append(f.aisles, named{fmt.Sprint(a), pt{x0 + n*bayW + 300, y}})
		for i := range n {
			x := x0 + (n-1-i)*bayW
			f.bays = append(f.bays,
				bay{a, i + 1, rect{x, y - 2000, x + bayW, y - 1400}, pt{0, 1}, false},
				bay{a, i + 11, rect{x, y + 1400, x + bayW, y + 2000}, pt{0, -1}, false})
		}
	}
	if top {
		y := min(y0, y0+(count-1)*step)
		for i := range n {
			x := x0 + i*bayW
			f.bays = append(f.bays, bay{0, 0, rect{x, y - 2600, x + bayW, y - 2000}, pt{0, -1}, false})
		}
	}
}

// down adds a block of aisles running up and down from y1 (the near end) to
// y1 - 4 bays: aisle first at x0, the next aisleW to its right, four bays a
// side numbered from the near end, 1-4 on the left and 5-8 on the right.
func (f *floor) down(first, count, x0, y1 int) {
	for k := range count {
		a, x := first+k, x0+k*aisleW
		f.aisles = append(f.aisles, named{fmt.Sprint(a), pt{x, y1 + 800}})
		for i := range 4 {
			y := y1 - (i+1)*bayW
			f.bays = append(f.bays,
				bay{a, i + 1, rect{x - 2000, y, x - 1400, y + bayW}, pt{1, 0}, false},
				bay{a, i + 5, rect{x + 1400, y, x + 2000, y + bayW}, pt{-1, 0}, false})
		}
	}
}

func main() {
	demo := flag.String("demo", "", "run check_list against the store and write the results to this directory")
	flag.Parse()
	files := build()
	if *demo != "" {
		if err := runDemo(*demo, files); err != nil {
			fmt.Fprintln(os.Stderr, "example-store:", err)
			os.Exit(1)
		}
		return
	}
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: example-store <testdata dir> | -demo <dir>")
		os.Exit(2)
	}
	for _, kind := range []string{"map", "nodes", "edges"} {
		write(filepath.Join(flag.Arg(0), store+"-"+kind+".json.gz"), files[kind])
	}
}

// build returns the store's map, nodes and edges files, as JSON.
func build() map[string][]byte {
	lower, ground := lowerGround(), groundFloor()
	floors := []*floor{lower, ground}

	// Level access: one link per pair of ends, a metre apart in plan as the
	// viewer's are, with y negated like the rest of the graph.
	links := [][2]struct {
		p      pt
		storey int
	}{
		{{pt{41000, 8000}, 2}, {pt{42000, 8000}, 1}},   // travelator
		{{pt{50000, 17000}, 2}, {pt{49000, 17000}, 1}}, // lift
	}

	type edge [2][2]int
	var nodeOut, edgeOut []map[string]any
	var level [][][3]int
	walkable := map[int]map[pt]bool{}
	for _, f := range floors {
		walkable[f.number] = f.grid()
	}
	// Keep the part of the grid that can be walked to from the main entrance.
	reach := map[int]map[pt]bool{1: {}, 2: {}}
	type sp struct {
		p pt
		s int
	}
	start := nearestOn(walkable[2], ground.entrances[0].at)
	queue := []sp{{start, 2}}
	reach[2][start] = true
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		next := []sp{}
		for _, d := range []pt{{grid, 0}, {-grid, 0}, {0, grid}, {0, -grid}} {
			next = append(next, sp{pt{c.p.X + d.X, c.p.Y + d.Y}, c.s})
		}
		for _, l := range links {
			for i, e := range l {
				if e.p == c.p && e.storey == c.s {
					next = append(next, sp{l[1-i].p, l[1-i].storey})
				}
			}
		}
		for _, n := range next {
			if walkable[n.s][n.p] && !reach[n.s][n.p] {
				reach[n.s][n.p] = true
				queue = append(queue, n)
			}
		}
	}
	for _, l := range links {
		for _, e := range l {
			if !reach[e.storey][e.p] {
				panic(fmt.Sprintf("level access end %v on %d is off the grid", e.p, e.storey))
			}
		}
		level = append(level, [][3]int{{l[0].p.X, -l[0].p.Y, l[0].storey}, {l[1].p.X, -l[1].p.Y, l[1].storey}})
	}

	for _, f := range floors {
		on := reach[f.number]
		var edges []edge
		for _, p := range sortedPoints(on) {
			for _, d := range []pt{{grid, 0}, {0, grid}} {
				q := pt{p.X + d.X, p.Y + d.Y}
				if on[q] {
					edges = append(edges, edge{{p.X, -p.Y}, {q.X, -q.Y}})
				}
			}
		}
		nodes := map[string][2]int{}
		for _, b := range f.bays {
			if b.aisle == 0 {
				continue
			}
			c := centre(b.r)
			// Out from the face into the aisle, as the viewer attaches bays.
			target := pt{c.X + b.face.X*1300, c.Y + b.face.Y*1300}
			p := nearestOn(on, target)
			nodes[fmt.Sprintf("%03d.%03d", b.aisle, b.n)] = [2]int{p.X, -p.Y}
		}
		for _, e := range f.entrances {
			p := nearestOn(on, e.at)
			nodes["entrance:"+lowerASCII(e.name)] = [2]int{p.X, -p.Y}
		}
		for _, l := range f.landmarks {
			p := nearestOn(on, centre(l.r))
			nodes[lowerASCII(l.name)] = [2]int{p.X, -p.Y}
		}
		nodeOut = append(nodeOut, map[string]any{"storey": f.number, "data": nodes})
		edgeOut = append(edgeOut, map[string]any{"storey": f.number, "data": edges})
	}

	var storeys []any
	for _, f := range floors {
		storeys = append(storeys, f.json())
	}
	return map[string][]byte{
		"map":   marshal(map[string]any{"mapId": mapID, "storeNumber": store, "storeName": name, "storeys": storeys}),
		"nodes": marshal(map[string]any{"resolution": 1, "storeys": nodeOut}),
		"edges": marshal(map[string]any{"resolution": 1, "storeys": edgeOut, "levelAccesses": level}),
	}
}

// groundFloor is storey 2: the entrance, registers and desks across the front
// (top), six aisles across on the left, two garden aisles up and down on the
// right, outdoor ranges at the back right, and a nursery wing.
func groundFloor() *floor {
	f := &floor{number: 2, name: "Ground", abbr: "G",
		outline: []pt{{-14000, -15000}, {54000, -15000}, {54000, 21000}, {0, 21000}, {0, 6000}, {-14000, 6000}}}
	// Aisle 1 at the back (bottom), 6 nearest the front.
	f.across(1, 6, 3000, 19000, -aisleW, true)
	// Garden aisles up and down on the right, below the entrance.
	f.down(7, 2, 46000, 6400)
	// Promotional end displays facing the main walkway.
	for k := range 6 {
		y := 17000 - k*aisleW
		f.bays = append(f.bays, bay{130, k + 1, rect{30000, y - 600, 30600, y + 600}, pt{1, 0}, true})
	}
	f.landmarks = []area{
		{"Registers_2", rect{22000, -12000, 30000, -10000}},
		{"Information Desk_2", rect{50000, -9000, 53000, -7500}},
		{"Paint Desk_2", rect{4000, -12000, 8000, -10000}},
	}
	f.departments = []named{
		{"Paint_2", pt{6000, -7000}}, {"Cleaning_2", pt{17000, -7000}},
		{"Garden Care_2", pt{48000, 9300}}, {"Outdoor  Living_2", pt{47000, 14500}},
		{"Nursery_2", pt{-7000, -6000}}, {"Indoor Plants_2", pt{-7000, 2000}},
	}
	f.entrances = []named{{"Main Entrance", pt{48000, -15000}}, {"Nursery Entrance", pt{-14000, -4000}}}
	f.level = []area{
		{"Travelator-0e1c5a7d2b9f4c3a8e6d1f0b2a4c6e81", rect{40000, 9000, 42000, 15000}},
		{"Lift-7b3e9d1c5a2f4e6b8d0c2a4e6f8b0d13", rect{50500, 15500, 53500, 18500}},
	}
	return f
}

// lowerGround is storey 1: six aisles across on the left, four up and down
// on the right, timber along the right-hand wall.
func lowerGround() *floor {
	f := &floor{number: 1, name: "Lower Ground", abbr: "LG",
		outline: []pt{{0, -5000}, {54000, -5000}, {54000, 19000}, {0, 19000}}}
	// Aisle 10 at the top, 15 at the back (bottom).
	f.across(10, 6, 3000, -3000, aisleW, false)
	f.down(20, 4, 36000, 6400)
	// Timber racking along the right-hand wall, unnumbered.
	for i := range 3 {
		y := -1000 + i*bayW
		f.bays = append(f.bays, bay{0, 0, rect{53000, y, 53600, y + bayW}, pt{-1, 0}, false})
	}
	for k := range 5 {
		y := 15000 - k*aisleW
		f.bays = append(f.bays, bay{140, k + 1, rect{30000, y - 600, 30600, y + 600}, pt{1, 0}, true})
	}
	// A display that shares a racking bay's number upstairs: Find must
	// prefer the racking.
	f.bays = append(f.bays, bay{2, 1, rect{1000, 18000, 2500, 18600}, pt{0, -1}, true})
	f.landmarks = []area{
		{"Toilets_1", rect{51000, -5000, 54000, -2000}},
		{"Special Orders Desk_1", rect{44000, 9500, 48000, 11000}},
	}
	f.departments = []named{
		{"Hardware_1", pt{16500, -3000}}, {"Plumbing_1", pt{16500, 9000}},
		{"Tools_1", pt{16500, 17000}}, {"Building Supplies_1", pt{46000, 13500}},
		{"Timber_1", pt{51600, 3000}},
	}
	f.level = []area{
		{"Travelator-4d8a2c6e0b1f3a5c7e9d2b4f6a8c0e35", rect{40000, 9000, 42000, 15000}},
		{"Lift-9c1e3a5b7d0f2c4e6a8b1d3f5a7c9e02", rect{50500, 15500, 53500, 18500}},
	}
	return f
}

// grid is the walking grid: points inside the outline and clear of
// everything drawn on the floor.
func (f *floor) grid() map[pt]bool {
	lo, hi := pt{math.MaxInt, math.MaxInt}, pt{math.MinInt, math.MinInt}
	for _, p := range f.outline {
		lo, hi = pt{min(lo.X, p.X), min(lo.Y, p.Y)}, pt{max(hi.X, p.X), max(hi.Y, p.Y)}
	}
	var blocked []rect
	for _, b := range f.bays {
		blocked = append(blocked, b.r)
	}
	for _, a := range append(append([]area{}, f.landmarks...), f.level...) {
		blocked = append(blocked, a.r)
	}
	out := map[pt]bool{}
	for x := lo.X; x <= hi.X; x += grid {
		for y := lo.Y; y <= hi.Y; y += grid {
			p := pt{x, y}
			if !inside(f.outline, p) || edgeDist(f.outline, p) < 2*clear {
				continue
			}
			ok := true
			for _, r := range blocked {
				if p.X > r.x0-clear && p.X < r.x1+clear && p.Y > r.y0-clear && p.Y < r.y1+clear {
					ok = false
					break
				}
			}
			if ok {
				out[p] = true
			}
		}
	}
	return out
}

func (f *floor) json() map[string]any {
	s := f.number
	p3 := func(p pt) []int { return []int{p.X, p.Y, s} }
	poly := func(ps []pt) [][]int {
		out := make([][]int, len(ps))
		for i, p := range ps {
			out[i] = p3(p)
		}
		return out
	}
	outline := poly(f.outline)
	aisles := []any{}
	for _, a := range f.aisles {
		aisles = append(aisles, []any{a.name, p3(a.at)})
	}
	bays, promo := []any{}, []any{}
	for _, b := range f.bays {
		var a, n any
		if b.aisle > 0 {
			a, n = b.aisle, b.n
		}
		row := []any{a, n, poly(b.r.poly()), (b.r.x1 - b.r.x0) * (b.r.y1 - b.r.y0)}
		if b.promo {
			promo = append(promo, row)
		} else {
			bays = append(bays, row)
		}
	}
	areas := func(as []area) []any {
		out := []any{}
		for _, a := range as {
			out = append(out, []any{a.name, poly(a.r.poly()), (a.r.x1 - a.r.x0) * (a.r.y1 - a.r.y0)})
		}
		return out
	}
	labels := func(ls []named) []any {
		out := []any{}
		for _, l := range ls {
			out = append(out, []any{l.name, p3(l.at)})
		}
		return out
	}
	return map[string]any{
		"storeyNumber": s, "name": f.name, "nameAbbreviated": f.abbr, "outline": outline,
		"aisles": aisles, "bays": bays, "promoBays": promo, "landmarks": areas(f.landmarks),
		"departmentLabels": labels(f.departments), "entrances": labels(f.entrances),
		"pointsOfInterest": []any{}, "levelAccess": areas(f.level),
	}
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func write(path string, b []byte) {
	var buf bytes.Buffer
	z, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	z.Write(b)
	z.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("%s: %d bytes, %d gzipped\n", path, len(b), buf.Len())
}

func centre(r rect) pt { return pt{(r.x0 + r.x1) / 2, (r.y0 + r.y1) / 2} }

func nearestOn(on map[pt]bool, t pt) pt {
	best, bd := pt{}, math.Inf(1)
	for _, p := range sortedPoints(on) {
		if d := math.Hypot(float64(p.X-t.X), float64(p.Y-t.Y)); d < bd {
			best, bd = p, d
		}
	}
	return best
}

func sortedPoints(on map[pt]bool) []pt {
	out := make([]pt, 0, len(on))
	for p := range on {
		out = append(out, p)
	}
	// Row by row, so the files come out the same every run.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func less(a, b pt) bool { return a.Y < b.Y || a.Y == b.Y && a.X < b.X }

func inside(poly []pt, p pt) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.Y > p.Y) != (b.Y > p.Y) && float64(p.X) < float64(b.X-a.X)*float64(p.Y-a.Y)/float64(b.Y-a.Y)+float64(a.X) {
			in = !in
		}
	}
	return in
}

func edgeDist(poly []pt, p pt) float64 {
	d := math.Inf(1)
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		dx, dy := float64(b.X-a.X), float64(b.Y-a.Y)
		t := math.Max(0, math.Min(1, (float64(p.X-a.X)*dx+float64(p.Y-a.Y)*dy)/(dx*dx+dy*dy)))
		d = math.Min(d, math.Hypot(float64(a.X)+t*dx-float64(p.X), float64(a.Y)+t*dy-float64(p.Y)))
	}
	return d
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
