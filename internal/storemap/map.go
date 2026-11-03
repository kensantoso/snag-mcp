// Package storemap reads Bunnings' own store floor plans, plans a walk through
// a store's bays, and draws it.
//
// The plans are the ones behind Bunnings' map viewer
// (map-viewer.apps.bunnings.com.au): per store, a map of every bay keyed by
// aisle and bay number, and a walking graph the viewer routes over. Their
// coordinates are millimetres with y increasing down the page, as on the
// printed store maps; x and y are kept as they come and only translated when
// drawn.
package storemap

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Point is a position on a storey, in millimetres, y down.
type Point struct{ X, Y float64 }

// Addr is an aisle and bay, the key the map and the item-location API share.
type Addr struct{ Aisle, Bay int }

// String formats an address as the viewer does: "022.002".
func (a Addr) String() string { return fmt.Sprintf("%03d.%03d", a.Aisle, a.Bay) }

// Bay is one shelving bay.
type Bay struct {
	Addr
	Poly  []Point
	Promo bool // an end or promotional display rather than a racking bay
}

// Label is a named point: an aisle number, a department, an entrance.
type Label struct {
	Name string
	At   Point
}

// Area is a named polygon: toilets, registers, a desk.
type Area struct {
	Name string
	Poly []Point
}

// Storey is one floor of a store.
type Storey struct {
	Number      int
	Name        string
	Outline     []Point
	Min, Max    Point // bounding box of the outline
	Aisles      []Label
	Bays        []Bay
	Landmarks   []Area
	Departments []Label
	Entrances   []Label
	// LevelAccess are the lifts, stairs and travelators between storeys.
	LevelAccess []Area
}

// Map is a store's floor plan.
type Map struct {
	ID        string
	Store     string
	StoreName string
	Storeys   []*Storey
}

// Located is where an address is on the map.
type Located struct {
	Storey *Storey
	Bay    Bay
}

// Centre is the middle of the bay's bounding box.
func (l Located) Centre() Point { return centre(l.Bay.Poly) }

// Find returns the bay for an address, preferring racking bays to promotional
// displays that share its number.
func (m *Map) Find(a Addr) (Located, bool) {
	if a.Aisle <= 0 || a.Bay <= 0 {
		return Located{}, false
	}
	for _, promo := range []bool{false, true} {
		for _, s := range m.Storeys {
			for _, b := range s.Bays {
				if b.Addr == a && b.Promo == promo {
					return Located{s, b}, true
				}
			}
		}
	}
	return Located{}, false
}

// Storey returns the storey with the given number.
func (m *Map) Storey(n int) *Storey {
	for _, s := range m.Storeys {
		if s.Number == n {
			return s
		}
	}
	return nil
}

// MainEntrance is where a walk starts: the entrance named "Main", else the
// first entrance on the ground storey.
func (m *Map) MainEntrance() (*Storey, Label, bool) {
	for _, s := range m.Storeys {
		for _, e := range s.Entrances {
			if strings.Contains(strings.ToLower(e.Name), "main") {
				return s, e, true
			}
		}
	}
	for _, s := range m.Storeys {
		if len(s.Entrances) > 0 {
			return s, s.Entrances[0], true
		}
	}
	return nil, Label{}, false
}

// Registers returns the registers nearest p on storey s.
func (s *Storey) Registers(p Point) (Area, bool) {
	best, found := Area{}, false
	for _, a := range s.Landmarks {
		if !strings.EqualFold(a.Name, "Registers") {
			continue
		}
		if !found || dist(centre(a.Poly), p) < dist(centre(best.Poly), p) {
			best, found = a, true
		}
	}
	return best, found
}

// ParseMap reads a store's -map.json.
func ParseMap(b []byte) (*Map, error) {
	var raw struct {
		MapID       string `json:"mapId"`
		StoreNumber string `json:"storeNumber"`
		StoreName   string `json:"storeName"`
		Storeys     []struct {
			StoreyNumber     int               `json:"storeyNumber"`
			Name             string            `json:"name"`
			Outline          [][]float64       `json:"outline"`
			Aisles           []json.RawMessage `json:"aisles"`
			Bays             []json.RawMessage `json:"bays"`
			PromoBays        []json.RawMessage `json:"promoBays"`
			Landmarks        []json.RawMessage `json:"landmarks"`
			DepartmentLabels []json.RawMessage `json:"departmentLabels"`
			Entrances        []json.RawMessage `json:"entrances"`
			PointsOfInterest []json.RawMessage `json:"pointsOfInterest"`
			LevelAccess      []json.RawMessage `json:"levelAccess"`
		} `json:"storeys"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("store map: %w", err)
	}
	if len(raw.Storeys) == 0 {
		return nil, fmt.Errorf("store map: no storeys")
	}
	m := &Map{ID: raw.MapID, Store: raw.StoreNumber, StoreName: raw.StoreName}
	for _, rs := range raw.Storeys {
		s := &Storey{Number: rs.StoreyNumber, Name: rs.Name, Outline: points(rs.Outline)}
		if len(s.Outline) < 3 {
			return nil, fmt.Errorf("store map: storey %d has no outline", rs.StoreyNumber)
		}
		s.Min, s.Max = bounds(s.Outline)
		// Drawing and the coordinates we report both divide by its size.
		if !(s.Max.X > s.Min.X && s.Max.Y > s.Min.Y) {
			return nil, fmt.Errorf("store map: storey %d outline has no area", rs.StoreyNumber)
		}
		for _, t := range rs.Aisles {
			if l, ok := label(t); ok {
				s.Aisles = append(s.Aisles, l)
			}
		}
		for _, list := range []struct {
			raw   []json.RawMessage
			promo bool
		}{{rs.Bays, false}, {rs.PromoBays, true}} {
			for _, t := range list.raw {
				if b, ok := bay(t); ok {
					b.Promo = list.promo
					s.Bays = append(s.Bays, b)
				}
			}
		}
		for _, t := range rs.Landmarks {
			if a, ok := area(t); ok {
				s.Landmarks = append(s.Landmarks, a)
			}
		}
		for _, t := range rs.DepartmentLabels {
			if l, ok := label(t); ok {
				s.Departments = append(s.Departments, l)
			}
		}
		for _, t := range rs.LevelAccess {
			if a, ok := area(t); ok {
				// "Lift-b8ed75ee..." is a lift.
				if i := strings.IndexByte(a.Name, '-'); i > 0 {
					a.Name = a.Name[:i]
				}
				s.LevelAccess = append(s.LevelAccess, a)
			}
		}
		for _, t := range rs.Entrances {
			if l, ok := label(t); ok {
				s.Entrances = append(s.Entrances, l)
			}
		}
		// As the viewer does: a map with no entrances has them among its
		// points of interest or landmarks instead.
		if len(s.Entrances) == 0 {
			for _, t := range rs.PointsOfInterest {
				if l, ok := label(t); ok && isEntrance(l.Name) {
					s.Entrances = append(s.Entrances, l)
				}
			}
			kept := s.Landmarks[:0]
			for _, a := range s.Landmarks {
				if isEntrance(a.Name) {
					s.Entrances = append(s.Entrances, Label{a.Name, centre(a.Poly)})
					continue
				}
				kept = append(kept, a)
			}
			s.Landmarks = kept
		}
		m.Storeys = append(m.Storeys, s)
	}
	return m, nil
}

var entranceRE = regexp.MustCompile(`(?i)entrance`)

func isEntrance(name string) bool { return entranceRE.MatchString(name) }

// DisplayName strips the "_1" suffix the map gives every landmark and
// department, and the doubled spaces some of them have, as the viewer does.
func DisplayName(s string) string {
	if i := strings.IndexByte(s, '_'); i >= 0 {
		s = s[:i]
	}
	return strings.Join(strings.Fields(s), " ")
}

func label(t json.RawMessage) (Label, bool) {
	var v []json.RawMessage
	if json.Unmarshal(t, &v) != nil || len(v) < 2 {
		return Label{}, false
	}
	var name string
	var p []float64
	if json.Unmarshal(v[0], &name) != nil || json.Unmarshal(v[1], &p) != nil || len(p) < 2 {
		return Label{}, false
	}
	return Label{DisplayName(name), Point{p[0], p[1]}}, true
}

func area(t json.RawMessage) (Area, bool) {
	var v []json.RawMessage
	if json.Unmarshal(t, &v) != nil || len(v) < 2 {
		return Area{}, false
	}
	var name string
	var p [][]float64
	if json.Unmarshal(v[0], &name) != nil || json.Unmarshal(v[1], &p) != nil || len(p) < 3 {
		return Area{}, false
	}
	return Area{DisplayName(name), points(p)}, true
}

// bay reads [aisle, bay, polygon, area]. Bays without both numbers are
// shelving the map does not address, and are kept for drawing as aisle 0.
func bay(t json.RawMessage) (Bay, bool) {
	var v []json.RawMessage
	if json.Unmarshal(t, &v) != nil || len(v) < 3 {
		return Bay{}, false
	}
	var a, b *float64
	var p [][]float64
	json.Unmarshal(v[0], &a)
	json.Unmarshal(v[1], &b)
	if json.Unmarshal(v[2], &p) != nil || len(p) < 3 {
		return Bay{}, false
	}
	out := Bay{Poly: points(p)}
	if a != nil && b != nil && *a > 0 && *b > 0 {
		out.Addr = Addr{int(*a), int(*b)}
	}
	return out, true
}

func points(p [][]float64) []Point {
	out := make([]Point, 0, len(p))
	for _, q := range p {
		if len(q) >= 2 {
			out = append(out, Point{q[0], q[1]})
		}
	}
	return out
}

func bounds(ps []Point) (Point, Point) {
	lo := Point{math.Inf(1), math.Inf(1)}
	hi := Point{math.Inf(-1), math.Inf(-1)}
	for _, p := range ps {
		lo.X, lo.Y = math.Min(lo.X, p.X), math.Min(lo.Y, p.Y)
		hi.X, hi.Y = math.Max(hi.X, p.X), math.Max(hi.Y, p.Y)
	}
	return lo, hi
}

func centre(ps []Point) Point {
	lo, hi := bounds(ps)
	return Point{(lo.X + hi.X) / 2, (lo.Y + hi.Y) / 2}
}

func dist(a, b Point) float64 { return math.Hypot(a.X-b.X, a.Y-b.Y) }
