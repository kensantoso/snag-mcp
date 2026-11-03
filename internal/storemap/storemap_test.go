package storemap

import (
	"bytes"
	"compress/gzip"
	"image/png"
	"io"
	"math"
	"os"
	"slices"
	"testing"
)

// exampleStore is Example Store (0001), an invented two-storey store written
// by scripts/example-store in the map viewer's formats: Lower Ground (storey
// 1) and Ground (storey 2), joined by a travelator and a lift, with the main
// entrance and the registers on Ground.
func exampleStore(t *testing.T) (*Map, *Graph) {
	t.Helper()
	read := func(name string) []byte {
		f, err := os.Open("testdata/0001-" + name + ".json.gz")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(z)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	m, err := ParseMap(read("map"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := ParseGraph(read("nodes"), read("edges"))
	if err != nil {
		t.Fatal(err)
	}
	return m, g
}

func find(t *testing.T, m *Map, a Addr) Located {
	t.Helper()
	l, ok := m.Find(a)
	if !ok {
		t.Fatalf("%v not on the map", a)
	}
	return l
}

func TestParse(t *testing.T) {
	m, _ := exampleStore(t)
	if m.Store != "0001" || m.ID != "0001-0001" || len(m.Storeys) != 2 {
		t.Fatalf("got store %q map %q with %d storeys", m.Store, m.ID, len(m.Storeys))
	}
	lg, g := m.Storey(1), m.Storey(2)
	if lg.Name != "Lower Ground" || g.Name != "Ground" {
		t.Errorf("storey names %q, %q", lg.Name, g.Name)
	}
	s, e, ok := m.MainEntrance()
	if !ok || s != g || e.Name != "Main Entrance" {
		t.Errorf("main entrance = %v on %v", e, s)
	}
	// "Registers_2" displays as "Registers", "Lift-9c1e..." as "Lift",
	// "Outdoor  Living_2" as "Outdoor Living".
	if _, ok := g.Registers(e.At); !ok {
		t.Error("no registers on Ground")
	}
	for _, s := range m.Storeys {
		if len(s.LevelAccess) != 2 || s.LevelAccess[0].Name != "Travelator" || s.LevelAccess[1].Name != "Lift" {
			t.Errorf("level access on %s = %v", s.Name, s.LevelAccess)
		}
	}
	if !slices.ContainsFunc(g.Departments, func(l Label) bool { return l.Name == "Outdoor Living" }) {
		t.Errorf("departments = %v", g.Departments)
	}
}

func TestEntrancesFromLandmarks(t *testing.T) {
	// The viewer's rule for maps with no entrances: take them from points of
	// interest and landmarks named like one, and strip the _N suffix.
	m, err := ParseMap([]byte(`{"mapId":"1-1","storeNumber":"1","storeName":"T","storeys":[{
		"storeyNumber":1,"name":"Ground","outline":[[0,-100,1],[100,-100,1],[100,0,1],[0,0,1]],
		"aisles":[],"bays":[],"promoBays":[],"departmentLabels":[],"entrances":[],"levelAccess":[],
		"pointsOfInterest":[["Trade Entrance_1",[10,-5,1]],["ATM_1",[50,-50,1]]],
		"landmarks":[["Main Entrance_1",[[40,-10,1],[60,-10,1],[60,0,1],[40,0,1]],200],["Registers_1",[[40,-30,1],[60,-30,1],[60,-20,1],[40,-20,1]],200]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := m.Storeys[0]
	var names []string
	for _, e := range s.Entrances {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"Trade Entrance", "Main Entrance"}) {
		t.Errorf("entrances = %q", names)
	}
	if len(s.Landmarks) != 1 || s.Landmarks[0].Name != "Registers" {
		t.Errorf("landmarks = %v", s.Landmarks)
	}
	if _, e, _ := m.MainEntrance(); e.At != (Point{50, -5}) {
		t.Errorf("main entrance at %v, want the landmark's centre", e.At)
	}
}

func TestFind(t *testing.T) {
	m, _ := exampleStore(t)
	if l := find(t, m, Addr{2, 1}); l.Storey.Number != 2 || l.Bay.Promo {
		t.Errorf("2/1 on storey %d, promo %v", l.Storey.Number, l.Bay.Promo)
	}
	if l := find(t, m, Addr{10, 3}); l.Storey.Number != 1 {
		t.Errorf("10/3 on storey %d, want Lower Ground", l.Storey.Number)
	}
	// Promotional displays are addressable too, when nothing else has the number.
	if l := find(t, m, Addr{140, 1}); !l.Bay.Promo {
		t.Error("140/1 should be a promo bay")
	}
	for _, a := range []Addr{{0, 0}, {2, 0}, {10, 999}, {99, 1}} {
		if _, ok := m.Find(a); ok {
			t.Errorf("%v found", a)
		}
	}
	if (Addr{22, 2}).String() != "022.002" {
		t.Error("address format")
	}
}

// The plan's y already runs down the page, as on Bunnings' printed maps:
// flipping it turns the store upside down. Example Store is drawn the same
// way, with the entrance at the top right of Ground and aisle 1 below aisle 4;
// its y values straddle zero, so a flip or a lost sign shows.
func TestOrientation(t *testing.T) {
	m, _ := exampleStore(t)
	g := m.Storey(2)
	_, e, _ := m.MainEntrance()
	fx := (e.At.X - g.Min.X) / (g.Max.X - g.Min.X)
	fy := (e.At.Y - g.Min.Y) / (g.Max.Y - g.Min.Y)
	if fx < 0.7 || fy > 0.3 {
		t.Errorf("entrance at %.2f across, %.2f down; want top right", fx, fy)
	}
	y := map[string]float64{}
	for _, a := range g.Aisles {
		y[a.Name] = a.At.Y
	}
	if !(y["1"] > y["4"]) {
		t.Errorf("aisle 1 at y %.0f, aisle 4 at %.0f; want 1 lower on the page", y["1"], y["4"])
	}
}

func TestPlanAcrossFloors(t *testing.T) {
	m, g := exampleStore(t)
	// Two bays upstairs (Ground, where the entrance is), two downstairs,
	// given interleaved.
	bays := []Located{find(t, m, Addr{10, 3}), find(t, m, Addr{2, 1}), find(t, m, Addr{21, 1}), find(t, m, Addr{4, 2})}
	w := Plan(m, g, bays)
	if w.Method != ByWalkingGraph || w.Start == nil || w.End == nil || w.End.Name != "Registers" {
		t.Fatalf("method %s, start %v, end %v", w.Method, w.Start, w.End)
	}
	var floors []int
	for _, s := range w.Stops {
		floors = append(floors, s.Storey.Number)
	}
	// One trip down and back up, not up and down for each bay.
	if !slices.Equal(floors, []int{2, 2, 1, 1}) && !slices.Equal(floors, []int{1, 1, 2, 2}) {
		t.Errorf("floors in walk order = %v", floors)
	}
	if len(w.Legs) != 5 || w.WalkM <= 0 {
		t.Fatalf("%d legs, %.0f m", len(w.Legs), w.WalkM)
	}
	var sum float64
	for _, s := range w.Stops {
		if s.LegM <= 0 {
			t.Errorf("stop %v leg %.1f m", s.Bay.Addr, s.LegM)
		}
		sum += s.LegM
	}
	if sum >= w.WalkM {
		t.Errorf("legs to stops %.1f m should be less than the walk %.1f m, which ends at the registers", sum, w.WalkM)
	}
	// Each leg ends at its bay, on the bay's floor: the graph and the plan
	// share coordinates once the graph's y is negated.
	for i, s := range w.Stops {
		end := w.Legs[i][len(w.Legs[i])-1]
		if end.Storey != s.Storey.Number || dist(end.Point, s.Centre()) > 3000 {
			t.Errorf("leg to %v ends at %v on floor %d, %.0f mm from the bay", s.Bay.Addr, end.Point, end.Storey, dist(end.Point, s.Centre()))
		}
	}
	onFloor := map[int]bool{}
	for _, leg := range w.Legs {
		for _, p := range leg {
			onFloor[p.Storey] = true
		}
	}
	if !onFloor[1] || !onFloor[2] {
		t.Errorf("walk drawn on floors %v", onFloor)
	}
}

// Retrace, given a plan's stops, gives back the same walk: what the view
// draws is the walk the pick list was ordered by. Given another order it
// keeps that order rather than replanning.
func TestRetrace(t *testing.T) {
	m, g := exampleStore(t)
	bays := []Located{find(t, m, Addr{10, 3}), find(t, m, Addr{2, 1}), find(t, m, Addr{21, 1}), find(t, m, Addr{4, 2})}
	w := Plan(m, g, bays)
	var planned []Located
	for _, s := range w.Stops {
		planned = append(planned, s.Located)
	}
	r := Retrace(m, g, planned)
	if r.Method != w.Method || math.Abs(r.WalkM-w.WalkM) > 1e-9 || len(r.Legs) != len(w.Legs) {
		t.Fatalf("retrace %s %.1f m %d legs, plan %s %.1f m %d legs", r.Method, r.WalkM, len(r.Legs), w.Method, w.WalkM, len(w.Legs))
	}
	for i := range w.Legs {
		if !slices.Equal(r.Legs[i], w.Legs[i]) {
			t.Errorf("leg %d differs", i)
		}
	}
	slices.Reverse(planned)
	r = Retrace(m, g, planned)
	for i, s := range r.Stops {
		if s.Bay.Addr != planned[i].Bay.Addr {
			t.Fatalf("stop %d is %v, given %v", i, s.Bay.Addr, planned[i].Bay.Addr)
		}
	}
	if r.WalkM <= w.WalkM {
		t.Errorf("the reversed walk, %.1f m, should be longer than the planned %.1f m", r.WalkM, w.WalkM)
	}
}

// corridor is a store where straight lines mislead: bay A is 10 m from the
// entrance as the crow flies, but a wall makes it a 70 m walk round by bay B,
// which is 20 m either way. No registers, so the walk ends at its last bay.
//
//	A . . . . +
//	##### wall :
//	E . . B . +
func corridor(t *testing.T) (*Map, *Graph) {
	t.Helper()
	m, err := ParseMap([]byte(`{"mapId":"9-1","storeNumber":"9","storeName":"Corridor","storeys":[{
		"storeyNumber":1,"name":"Ground","outline":[[0,-20000,1],[40000,-20000,1],[40000,0,1],[0,0,1]],
		"aisles":[],"promoBays":[],"landmarks":[],"departmentLabels":[],"pointsOfInterest":[],"levelAccess":[],
		"entrances":[["Main Entrance",[0,0,1]]],
		"bays":[[1,1,[[-500,-10500,1],[500,-10500,1],[500,-9500,1],[-500,-9500,1]],1],
		        [2,1,[[19500,-500,1],[20500,-500,1],[20500,500,1],[19500,500,1]],1]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Graph y is stored positive and negated on reading.
	g, err := ParseGraph(
		[]byte(`{"resolution":1,"data":{"001.001":[0,10000],"002.001":[20000,0],"entrance:main entrance":[0,0]}}`),
		[]byte(`{"resolution":1,"data":[[[0,0],[20000,0]],[[20000,0],[30000,0]],[[30000,0],[30000,10000]],[[30000,10000],[0,10000]]]}`))
	if err != nil {
		t.Fatal(err)
	}
	return m, g
}

func TestPlanUsesWalkingDistance(t *testing.T) {
	m, g := corridor(t)
	a, b := find(t, m, Addr{1, 1}), find(t, m, Addr{2, 1})
	w := Plan(m, g, []Located{a, b})
	if w.Method != ByWalkingGraph || w.Stops[0].Bay.Addr != (Addr{2, 1}) {
		t.Fatalf("%s: first stop %v, want 2/1 (nearer on foot)", w.Method, w.Stops[0].Bay.Addr)
	}
	if w.WalkM != 70 || w.Stops[0].LegM != 20 || w.Stops[1].LegM != 50 {
		t.Errorf("walk %.1f m, legs %.1f, %.1f; want 70 = 20 + 50", w.WalkM, w.Stops[0].LegM, w.Stops[1].LegM)
	}

	// Without a graph: straight lines, which go to A first, and no claim
	// about walking distance.
	w = Plan(m, nil, []Located{b, a})
	if w.Method != ByStraightLine || w.Stops[0].Bay.Addr != (Addr{1, 1}) || w.WalkM != 0 || len(w.Legs) != 0 {
		t.Errorf("straight line: %s, first %v, %.0f m, %d legs", w.Method, w.Stops[0].Bay.Addr, w.WalkM, len(w.Legs))
	}
}

func TestPlanUnreachableFallsBack(t *testing.T) {
	m, _ := corridor(t)
	// A graph with no way to bay A.
	g, err := ParseGraph(
		[]byte(`{"resolution":1,"data":{"002.001":[20000,0],"entrance:main entrance":[0,0]}}`),
		[]byte(`{"resolution":1,"data":[[[0,0],[20000,0]],[[0,10000],[1000,10000]]]}`))
	if err != nil {
		t.Fatal(err)
	}
	w := Plan(m, g, []Located{find(t, m, Addr{2, 1}), find(t, m, Addr{1, 1})})
	if w.Method != ByStraightLine || len(w.Stops) != 2 {
		t.Errorf("method %s with %d stops", w.Method, len(w.Stops))
	}
}

func TestOrderEndsAtRegisters(t *testing.T) {
	// On a line: entrance at 0, bays at -10 and +20, registers at -40. Open
	// ended, the near bay comes first (10+30 < 20+30); finishing at the
	// registers, the far one does (20+30+30 < 10+30+60).
	line := func(pts ...float64) [][]float64 {
		return matrix(len(pts), func(i, j int) float64 { return math.Abs(pts[i] - pts[j]) })
	}
	if got := order(line(0, -10, 20), false); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("open: %v", got)
	}
	if got := order(line(0, -10, 20, -40), true); !slices.Equal(got, []int{2, 1}) {
		t.Errorf("to registers: %v", got)
	}
}

func TestOrderManyStops(t *testing.T) {
	// Twenty points along a line, given shuffled, from one end: past the
	// exact solver's limit, the walk must still go straight along.
	pos := []float64{7, 3, 19, 1, 12, 5, 18, 2, 9, 15, 4, 11, 20, 6, 14, 8, 17, 10, 16, 13}
	pts := append([]float64{0}, pos...)
	cost := matrix(len(pts), func(i, j int) float64 {
		d := pts[i] - pts[j]
		if d < 0 {
			d = -d
		}
		return d
	})
	var got []float64
	for _, i := range order(cost, false) {
		got = append(got, pts[i])
	}
	want := slices.Clone(pos)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("order = %v", got)
	}
}

func TestRender(t *testing.T) {
	m, g := exampleStore(t)
	w := Plan(m, g, []Located{find(t, m, Addr{10, 3}), find(t, m, Addr{2, 1})})
	b, err := Render(m, w, Drawing{Title: "Example Store", Labels: []string{"one", "two"},
		Legend: []LegendLine{{1, "a"}, {2, "b"}, {0, "Not on map: c"}}})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != mapWidth+2*margin || img.Bounds().Dy() < 600 {
		t.Errorf("image %v", img.Bounds())
	}
	marked := 0
	for y := 0; y < img.Bounds().Dy(); y += 2 {
		for x := 0; x < img.Bounds().Dx(); x += 2 {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 == uint32(accent.R) && g>>8 == uint32(accent.G) && b>>8 == uint32(accent.B) {
				marked++
			}
		}
	}
	if marked < 100 {
		t.Errorf("only %d marker pixels", marked)
	}
}

// Malformed files are someone else's data: they must fail or be skipped, not
// panic a check_list.
func TestMalformed(t *testing.T) {
	// Level accesses too short to be a link, beside one good edge.
	g, err := ParseGraph(
		[]byte(`{"resolution":1,"data":{"001.001":[0,0]}}`),
		[]byte(`{"resolution":1,"data":[[[0,0],[1000,0]],[[5,5]],[]],
			"levelAccesses":[[],[[0,0,1]],[[0,0],[0,0,2]],[[0,0,1],[0,0]]]}`))
	if err != nil || len(g.nodes) != 2 {
		t.Fatalf("graph with bad level accesses: %v, %d nodes", err, len(g.nodes))
	}
	if _, err := ParseGraph([]byte(`{}`), []byte(`{"levelAccesses":[[[1]]]}`)); err == nil {
		t.Error("a graph with no usable edge parsed")
	}

	// An outline that is a line or a point has no area to draw.
	for _, outline := range []string{
		`[[0,0,1],[100,0,1],[50,0,1]]`,
		`[[7,7,1],[7,7,1],[7,7,1]]`,
	} {
		_, err := ParseMap([]byte(`{"mapId":"1-1","storeNumber":"1","storeys":[{"storeyNumber":1,"name":"G","outline":` + outline + `}]}`))
		if err == nil {
			t.Errorf("outline %s parsed", outline)
		}
	}
}

// Named nodes belong to a storey: the same name upstairs is another node.
func TestNamedNodesPerStorey(t *testing.T) {
	g, err := ParseGraph(
		[]byte(`{"resolution":1,"storeys":[{"storey":1,"data":{"lift":[0,0]}},{"storey":2,"data":{"lift":[9000,0]}}]}`),
		[]byte(`{"resolution":1,"storeys":[{"storey":1,"data":[[[0,0],[1000,0]]]},{"storey":2,"data":[[[9000,0],[8000,0]]]}],"levelAccesses":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	for storey, want := range map[int]Point{1: {0, 0}, 2: {9000, 0}} {
		if n := g.nodes[g.nodeFor("Lift", Point{500, 0}, storey)]; n.P != want || n.Storey != storey {
			t.Errorf("lift on storey %d = %v", storey, n)
		}
	}
}

// Markers and captions land on the canvas above the legend, clear of each
// other and of the entrance, finish and lift labels: in a crowd of
// neighbours at Example Store, and on a plan built to trap them, with a bay right
// where the finish label goes and bays against the walls.
func TestRenderLayoutStaysClear(t *testing.T) {
	m, g := exampleStore(t)
	var crowd []Located
	for _, a := range []Addr{{2, 1}, {2, 2}, {2, 3}, {2, 4}, {3, 1}, {3, 2}, {4, 1}, {4, 2}, {1, 1}, {1, 2}, {5, 1}, {10, 3}, {21, 1}, {6, 1}} {
		if l, ok := m.Find(a); ok {
			crowd = append(crowd, l)
		}
	}
	checkLayout(t, "Example Store", m, Plan(m, g, crowd))

	// 40 x 20 m; entrance bottom middle; registers just inside it; bay 1/1
	// 0.65 m above the registers, under where the finish label goes.
	trap, err := ParseMap([]byte(`{"mapId":"8-1","storeNumber":"8","storeName":"Trap","storeys":[{
		"storeyNumber":1,"name":"Ground","outline":[[0,0,1],[40000,0,1],[40000,20000,1],[0,20000,1]],
		"aisles":[],"promoBays":[],"departmentLabels":[],"pointsOfInterest":[],"levelAccess":[],
		"entrances":[["Main Entrance",[20000,20000,1]]],
		"landmarks":[["Registers_1",[[19000,16500,1],[21000,16500,1],[21000,17500,1],[19000,17500,1]],1]],
		"bays":[[1,1,[[19700,16200,1],[20300,16200,1],[20300,16500,1],[19700,16500,1]],1],
		        [1,2,[[19700,15900,1],[20300,15900,1],[20300,16200,1],[19700,16200,1]],1],
		        [2,1,[[0,0,1],[300,0,1],[300,300,1],[0,300,1]],1],
		        [3,1,[[39700,19700,1],[40000,19700,1],[40000,20000,1],[39700,20000,1]],1],
		        [4,1,[[39700,0,1],[40000,0,1],[40000,300,1],[39700,300,1]],1]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var bays []Located
	for _, a := range []Addr{{1, 1}, {1, 2}, {2, 1}, {3, 1}, {4, 1}} {
		bays = append(bays, find(t, trap, a))
	}
	// Placing markers clear of other markers only, as render first did,
	// nudges one onto the START label here.
	checkLayout(t, "trap", trap, Plan(trap, nil, bays))
}

func checkLayout(t *testing.T, name string, m *Map, w Walk) layout {
	t.Helper()
	labels := make([]string, len(w.Stops))
	for i := range labels {
		labels[i] = "Something rather long ×2"
	}
	_, lay, err := render(m, w, Drawing{Title: name, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if len(lay.markers) != len(w.Stops) || len(lay.fixed) < 2 || len(lay.captions) == 0 {
		t.Fatalf("%s: %d markers for %d stops, %d fixed labels, %d captions", name, len(lay.markers), len(w.Stops), len(lay.fixed), len(lay.captions))
	}
	onCanvas := func(r rect) bool {
		return r.x0 >= 0 && r.y0 >= 0 && r.x1 <= float64(lay.W) && r.y1 <= lay.legendTop
	}
	for i, r := range lay.markers {
		if !onCanvas(r) {
			t.Errorf("%s: marker %d at %v is off the plan (%dx%.0f)", name, i+1, r, lay.W, lay.legendTop)
		}
		for j, o := range lay.markers[:i] {
			if r.overlaps(o) {
				t.Errorf("%s: markers %d and %d overlap", name, j+1, i+1)
			}
		}
		for _, o := range lay.fixed {
			if r.overlaps(o) {
				t.Errorf("%s: marker %d covers a fixed label at %v", name, i+1, o)
			}
		}
	}
	for i, r := range lay.captions {
		if !onCanvas(r) {
			t.Errorf("%s: caption %d at %v is off the plan", name, i, r)
		}
		for _, o := range append(append(slices.Clone(lay.markers), lay.fixed...), lay.captions[:i]...) {
			if r.overlaps(o) {
				t.Errorf("%s: caption %d at %v overlaps %v", name, i, r, o)
			}
		}
	}
	return lay
}
