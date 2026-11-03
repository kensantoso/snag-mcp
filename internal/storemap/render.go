package storemap

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"slices"
	"strconv"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Drawing is what to draw over a store's plan.
type Drawing struct {
	Title    string
	Subtitle string
	// Labels are short captions for each stop of the walk, in walk order.
	Labels []string
	// Legend lines under the plan; Stop 0 is a line that is not on the map.
	Legend []LegendLine
	Footer string
}

// LegendLine is one line of the key under the plan.
type LegendLine struct {
	Stop int
	Text string
}

const (
	mapWidth   = 1500 // pixels across the plan itself
	maxPanelH  = 900
	maxMapsH   = 1300 // all storeys together
	margin     = 56
	titleH     = 64
	markerR    = 12
	legendRowH = 22
)

var (
	white     = color.RGBA{255, 255, 255, 255}
	floorFill = color.RGBA{244, 246, 246, 255}
	// Neutral on purpose: no retailer's brand colours.
	wallCol   = color.RGBA{52, 68, 76, 255}
	bayFill   = color.RGBA{201, 211, 214, 255}
	promoFill = color.RGBA{241, 220, 160, 255}
	landFill  = color.RGBA{226, 233, 234, 255}
	liftFill  = color.RGBA{214, 224, 238, 255}
	accent    = color.RGBA{43, 89, 195, 255}
	routeCol  = color.NRGBA{43, 89, 195, 140}
	aisleCol  = color.RGBA{93, 109, 114, 255}
	deptCol   = color.RGBA{120, 136, 141, 255}
	ink       = color.RGBA{30, 38, 40, 255}
	muted     = color.RGBA{110, 120, 124, 255}
)

type faces struct{ aisle, dept, label, legend, num, entrance, title, sub font.Face }

func loadFaces() (faces, error) {
	reg, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return faces{}, err
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return faces{}, err
	}
	face := func(f *opentype.Font, size float64) font.Face {
		fc, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
		return fc
	}
	return faces{
		aisle: face(reg, 11), dept: face(reg, 13), label: face(reg, 13), legend: face(reg, 15),
		num: face(bold, 13), entrance: face(bold, 13), title: face(bold, 22), sub: face(reg, 15),
	}, nil
}

type rect struct{ x0, y0, x1, y1 float64 }

func (r rect) overlaps(o rect) bool {
	return r.x0 < o.x1 && o.x0 < r.x1 && r.y0 < o.y1 && o.y0 < r.y1
}

type fpt struct{ x, y float64 }

// Render draws the storeys a walk visits, each stop's bay highlighted and
// numbered in walking order, as a PNG.
func Render(m *Map, w Walk, d Drawing) ([]byte, error) {
	b, _, err := render(m, w, d)
	return b, err
}

// layout is where render put things, for tests.
type layout struct {
	W, H      int
	legendTop float64
	markers   []rect
	anchors   []fpt // each marker's bay
	captions  []rect
	fixed     []rect // entrance, finish and lift labels
}

func render(m *Map, w Walk, d Drawing) ([]byte, layout, error) {
	var lay layout
	f, err := loadFaces()
	if err != nil {
		return nil, lay, err
	}
	// Storeys to draw: the entrance's, then the others in the order the walk
	// reaches them.
	var show []*Storey
	if w.Start != nil {
		show = append(show, m.Storey(w.StartStorey))
	}
	for _, st := range w.Stops {
		if !slices.Contains(show, st.Storey) {
			show = append(show, st.Storey)
		}
	}
	if len(show) == 0 || show[0] == nil {
		return nil, lay, fmt.Errorf("nothing to draw")
	}
	// One scale for every storey, so floors compare, and the whole image no
	// taller than a chat can show legibly.
	scale := math.Inf(1)
	var tall float64
	for _, s := range show {
		scale = math.Min(scale, mapWidth/(s.Max.X-s.Min.X))
		scale = math.Min(scale, maxPanelH/(s.Max.Y-s.Min.Y))
		tall += s.Max.Y - s.Min.Y
	}
	scale = math.Min(scale, maxMapsH/tall)
	type panel struct {
		s   *Storey
		top float64
	}
	var panels []panel
	y := float64(titleH)
	for _, s := range show {
		if len(show) > 1 {
			y += 24
		}
		panels = append(panels, panel{s, y + margin})
		y += (s.Max.Y-s.Min.Y)*scale + 2*margin
	}
	cols := 2
	rows := (len(d.Legend) + cols - 1) / cols
	legendTop := y + 8
	H := int(legendTop + float64(rows*legendRowH) + 44)
	W := mapWidth + 2*margin
	c := newCanvas(W, H)

	var placed []rect // everything drawn as text or marker, to avoid overlaps
	free := func(r rect) bool {
		if r.x0 < 2 || r.y0 < 2 || r.x1 > float64(W-2) || r.y1 > legendTop {
			return false
		}
		for _, p := range placed {
			if r.overlaps(p) {
				return false
			}
		}
		return true
	}

	c.text(f.title, d.Title, 20, 30, ink)
	c.text(f.sub, d.Subtitle, 20, 52, muted)

	type mark struct {
		n        int
		at, bay  fpt
		storey   *Storey
		hasLabel bool
		label    rect
		text     string
	}
	var marks []mark
	var texts []func() // drawn after shapes, in placement order

	for _, p := range panels {
		s := p.s
		tr := func(q Point) fpt {
			return fpt{margin + (q.X-s.Min.X)*scale, p.top + (q.Y-s.Min.Y)*scale}
		}
		poly := func(ps []Point) []fpt {
			out := make([]fpt, len(ps))
			for i, q := range ps {
				out[i] = tr(q)
			}
			return out
		}
		if len(show) > 1 {
			c.text(f.entrance, s.Name, margin, p.top-margin+4, ink)
		}
		c.fill([][]fpt{poly(s.Outline)}, floorFill)
		var lands, bays, promos [][]fpt
		for _, a := range s.Landmarks {
			lands = append(lands, poly(a.Poly))
		}
		for _, b := range s.Bays {
			if b.Promo {
				promos = append(promos, poly(b.Poly))
			} else {
				bays = append(bays, poly(b.Poly))
			}
		}
		c.fill(lands, landFill)
		c.fill(promos, promoFill)
		c.fill(bays, bayFill)
		c.stroke([][]fpt{append(poly(s.Outline), tr(s.Outline[0]))}, 2.5, wallCol)

		// Lifts and stairs, where a walk changes floor.
		var lifts [][]fpt
		for _, a := range s.LevelAccess {
			lifts = append(lifts, poly(a.Poly))
		}
		c.fill(lifts, liftFill)
		c.stroke(closed(lifts), 1.5, wallCol)
		for _, a := range s.LevelAccess {
			at := tr(centre(a.Poly))
			r := textRect(f.entrance, a.Name, at.x, at.y-8, "center")
			placed = append(placed, r)
			lay.fixed = append(lay.fixed, r)
			name := a.Name
			texts = append(texts, func() { c.textIn(f.entrance, name, r, wallCol) })
		}

		// The walk: each stretch on this floor.
		var legs [][]fpt
		for _, leg := range w.Legs {
			var run []fpt
			for _, q := range leg {
				if q.Storey == s.Number {
					run = append(run, tr(q.Point))
					continue
				}
				if len(run) > 1 {
					legs = append(legs, run)
				}
				run = nil
			}
			if len(run) > 1 {
				legs = append(legs, run)
			}
		}
		c.stroke(legs, 3, routeCol)

		var hi [][]fpt
		for i, st := range w.Stops {
			if st.Storey != s {
				continue
			}
			hi = append(hi, poly(st.Bay.Poly))
			b := tr(st.Centre())
			marks = append(marks, mark{n: i + 1, bay: b, storey: s})
		}
		c.fill(hi, accent)
		c.stroke(closed(hi), 2, accent)

		// Entrances, and where the walk starts and ends.
		for _, e := range s.Entrances {
			at := tr(e.At)
			name := e.Name
			if w.Start != nil && s.Number == w.StartStorey && e == *w.Start {
				name = "START: " + name
			}
			c.disc(at, 7, wallCol)
			r := textRect(f.entrance, name, at.x, at.y+12, "center")
			if !free(r) {
				r = textRect(f.entrance, name, at.x, at.y-12-(r.y1-r.y0), "center")
			}
			placed = append(placed, r, rect{at.x - 8, at.y - 8, at.x + 8, at.y + 8})
			lay.fixed = append(lay.fixed, r)
			texts = append(texts, func() { c.boxText(f.entrance, name, r, white, wallCol, wallCol) })
		}
		if w.End != nil && s.Number == w.StartStorey {
			at := tr(w.End.At)
			name := "FINISH: " + w.End.Name
			c.disc(at, 6, wallCol)
			// Above, below, then beside; on top of whatever is there if
			// nothing is free, since the finish must be shown.
			spots := captionSpots(f.entrance, name, at)
			r := spots[2]
			if k := slices.IndexFunc(spots, free); k >= 0 {
				r = spots[k]
			}
			placed = append(placed, r, rect{at.x - 7, at.y - 7, at.x + 7, at.y + 7})
			lay.fixed = append(lay.fixed, r)
			texts = append(texts, func() {
				c.stroke([][]fpt{{at, nearest(r, at)}}, 1, wallCol)
				c.boxText(f.entrance, name, r, white, wallCol, wallCol)
			})
		}
	}

	// Markers: on the bay if there is room, else nudged out with a leader.
	// Room means clear of other markers and of the entrance, finish and lift
	// labels already placed, and inside the plan. Failing that, clear of
	// other markers; failing that too, on the bay.
	box := func(p fpt) rect {
		return rect{p.x - markerR - 1, p.y - markerR - 1, p.x + markerR + 1, p.y + markerR + 1}
	}
	var mrects []rect
	for i := range marks {
		mk := &marks[i]
		mk.at = mk.bay
		clearOfMarkers := func(r rect) bool { return !slices.ContainsFunc(mrects, r.overlaps) }
		inside := func(r rect) bool { return r.x0 >= 2 && r.y0 >= 2 && r.x1 <= float64(W-2) && r.y1 <= legendTop }
		cands := ring(mk.bay)
		if k := slices.IndexFunc(cands, func(p fpt) bool { r := box(p); return clearOfMarkers(r) && free(r) }); k >= 0 {
			mk.at = cands[k]
		} else if k := slices.IndexFunc(cands, func(p fpt) bool { r := box(p); return clearOfMarkers(r) && inside(r) }); k >= 0 {
			mk.at = cands[k]
		}
		mrects = append(mrects, box(mk.at))
	}
	placed = append(placed, mrects...)
	lay.markers = mrects
	for _, mk := range marks {
		lay.anchors = append(lay.anchors, mk.bay)
	}
	// Captions beside markers, where they fit; the legend names every stop.
	for i := range marks {
		mk := &marks[i]
		if mk.n-1 >= len(d.Labels) || d.Labels[mk.n-1] == "" {
			continue
		}
		mk.text = d.Labels[mk.n-1]
		for _, r := range captionSpots(f.label, mk.text, mk.at) {
			if free(r) {
				mk.label, mk.hasLabel = r, true
				lay.captions = append(lay.captions, r)
				placed = append(placed, r)
				break
			}
		}
	}
	// Aisle numbers and departments fill in around them.
	for _, p := range panels {
		s := p.s
		for _, a := range s.Aisles {
			at := fpt{margin + (a.At.X-s.Min.X)*scale, p.top + (a.At.Y-s.Min.Y)*scale}
			r := textRect(f.aisle, a.Name, at.x, at.y-6, "center")
			if free(r) {
				placed = append(placed, r)
				name := a.Name
				texts = append([]func(){func() { c.textIn(f.aisle, name, r, aisleCol) }}, texts...)
			}
		}
		for _, dl := range s.Departments {
			at := fpt{margin + (dl.At.X-s.Min.X)*scale, p.top + (dl.At.Y-s.Min.Y)*scale}
			r := textRect(f.dept, dl.Name, at.x, at.y-7, "center")
			if free(r) {
				placed = append(placed, r)
				name := dl.Name
				texts = append([]func(){func() { c.textIn(f.dept, name, r, deptCol) }}, texts...)
			}
		}
	}
	for _, t := range texts {
		t()
	}
	for _, mk := range marks {
		if mk.at != mk.bay {
			c.stroke([][]fpt{{mk.bay, mk.at}}, 1.5, accent)
		}
	}
	for _, mk := range marks {
		if mk.hasLabel {
			c.stroke([][]fpt{{mk.at, nearest(mk.label, mk.at)}}, 1, accent)
			c.boxText(f.label, mk.text, mk.label, white, accent, ink)
		}
		c.disc(mk.at, markerR+2, white)
		c.disc(mk.at, markerR, accent)
		c.textIn(f.num, strconv.Itoa(mk.n), rect{mk.at.x - markerR, mk.at.y - markerR, mk.at.x + markerR, mk.at.y + markerR}, white)
	}

	// Legend.
	colW := float64(W-40) / float64(cols)
	for i, l := range d.Legend {
		x := 20 + float64(i/rows)*colW
		yy := legendTop + float64(i%rows)*legendRowH + 14
		tx := x + 30
		if l.Stop > 0 {
			c.disc(fpt{x + 11, yy - 5}, 10, accent)
			c.textIn(f.num, strconv.Itoa(l.Stop), rect{x + 1, yy - 15, x + 21, yy + 5}, white)
		}
		c.text(f.legend, fit(f.legend, l.Text, colW-40), tx, yy, ink)
	}
	c.text(f.aisle, d.Footer, 20, float64(H-12), muted)

	var buf bytes.Buffer
	if err := png.Encode(&buf, c.img); err != nil {
		return nil, lay, err
	}
	lay.W, lay.H, lay.legendTop = W, H, legendTop
	return buf.Bytes(), lay, nil
}

// ring is where a marker may go: on its bay, then in widening rings round it.
func ring(c fpt) []fpt {
	out := []fpt{c}
	for _, r := range []float64{22, 34, 48, 62} {
		for k := 0; k < 12; k++ {
			a := float64(k) * math.Pi / 6
			out = append(out, fpt{c.x + r*math.Cos(a), c.y + r*math.Sin(a)})
		}
	}
	return out
}

func captionSpots(f font.Face, s string, at fpt) []rect {
	var out []rect
	for _, gap := range []float64{4, 18, 34} {
		g := markerR + gap
		out = append(out,
			textBox(f, s, at.x+g, at.y, "left"),
			textBox(f, s, at.x-g, at.y, "right"),
			textBox(f, s, at.x, at.y-g-9, "center"),
			textBox(f, s, at.x, at.y+g+9, "center"),
			textBox(f, s, at.x+g, at.y-g, "left"),
			textBox(f, s, at.x+g, at.y+g, "left"),
			textBox(f, s, at.x-g, at.y-g, "right"),
			textBox(f, s, at.x-g, at.y+g, "right"),
		)
	}
	return out
}

// textBox is a padded caption box anchored at x by its left, right or centre
// and centred vertically on y.
func textBox(f font.Face, s string, x, y float64, anchor string) rect {
	w := float64(font.MeasureString(f, s).Ceil()) + 10
	h := float64((f.Metrics().Ascent + f.Metrics().Descent).Ceil()) + 4
	switch anchor {
	case "right":
		x -= w
	case "center":
		x -= w / 2
	}
	return rect{x, y - h/2, x + w, y + h/2}
}

// textRect is a tight box for bare text whose top is at y.
func textRect(f font.Face, s string, x, y float64, anchor string) rect {
	r := textBox(f, s, x, 0, anchor)
	h := r.y1 - r.y0
	return rect{r.x0, y, r.x1, y + h}
}

func nearest(r rect, p fpt) fpt {
	return fpt{math.Max(r.x0, math.Min(p.x, r.x1)), math.Max(r.y0, math.Min(p.y, r.y1))}
}

func fit(f font.Face, s string, w float64) string {
	if float64(font.MeasureString(f, s).Ceil()) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && float64(font.MeasureString(f, string(r)+"…").Ceil()) > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func closed(ps [][]fpt) [][]fpt {
	out := make([][]fpt, len(ps))
	for i, p := range ps {
		out[i] = append(slices.Clone(p), p[0])
	}
	return out
}

// ---------- canvas ----------

type canvas struct {
	img *image.RGBA
	z   *vector.Rasterizer
}

func newCanvas(w, h int) *canvas {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)
	return &canvas{img: img, z: vector.NewRasterizer(w, h)}
}

// fill paints polygons in one pass. Each is wound the same way, so where they
// overlap coverage adds rather than cancels.
func (c *canvas) fill(polys [][]fpt, col color.Color) {
	if len(polys) == 0 {
		return
	}
	// Rasterise only the box the polygons cover.
	x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, p := range polys {
		for _, q := range p {
			x0, y0, x1, y1 = math.Min(x0, q.x), math.Min(y0, q.y), math.Max(x1, q.x), math.Max(y1, q.y)
		}
	}
	b := image.Rect(int(math.Floor(x0)), int(math.Floor(y0)), int(math.Ceil(x1))+1, int(math.Ceil(y1))+1).Intersect(c.img.Bounds())
	if b.Empty() {
		return
	}
	ox, oy := float64(b.Min.X), float64(b.Min.Y)
	c.z.Reset(b.Dx(), b.Dy())
	for _, p := range polys {
		if len(p) < 3 {
			continue
		}
		if area2(p) < 0 {
			p = slices.Clone(p)
			slices.Reverse(p)
		}
		c.z.MoveTo(float32(p[0].x-ox), float32(p[0].y-oy))
		for _, q := range p[1:] {
			c.z.LineTo(float32(q.x-ox), float32(q.y-oy))
		}
		c.z.ClosePath()
	}
	c.z.DrawOp = draw.Over
	c.z.Draw(c.img, b, image.NewUniform(col), image.Point{})
}

// stroke paints polylines as a quad per segment plus round joints.
func (c *canvas) stroke(lines [][]fpt, width float64, col color.Color) {
	var polys [][]fpt
	h := width / 2
	for _, l := range lines {
		for i := 1; i < len(l); i++ {
			a, b := l[i-1], l[i]
			dx, dy := b.x-a.x, b.y-a.y
			n := math.Hypot(dx, dy)
			if n == 0 {
				continue
			}
			nx, ny := -dy/n*h, dx/n*h
			polys = append(polys, []fpt{{a.x + nx, a.y + ny}, {b.x + nx, b.y + ny}, {b.x - nx, b.y - ny}, {a.x - nx, a.y - ny}})
			if i > 1 {
				polys = append(polys, circle(a, h))
			}
		}
	}
	c.fill(polys, col)
}

func (c *canvas) disc(at fpt, r float64, col color.Color) { c.fill([][]fpt{circle(at, r)}, col) }

func circle(at fpt, r float64) []fpt {
	n := 24
	out := make([]fpt, n)
	for i := range out {
		a := 2 * math.Pi * float64(i) / float64(n)
		out[i] = fpt{at.x + r*math.Cos(a), at.y + r*math.Sin(a)}
	}
	return out
}

func area2(p []fpt) float64 {
	var s float64
	for i := range p {
		j := (i + 1) % len(p)
		s += p[i].x*p[j].y - p[j].x*p[i].y
	}
	return s
}

func (c *canvas) text(f font.Face, s string, x, baseline float64, col color.Color) {
	dr := font.Drawer{Dst: c.img, Src: image.NewUniform(col), Face: f,
		Dot: fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(baseline * 64)}}
	dr.DrawString(s)
}

// textIn centres text in r.
func (c *canvas) textIn(f font.Face, s string, r rect, col color.Color) {
	m := f.Metrics()
	w := float64(font.MeasureString(f, s).Ceil())
	asc, desc := float64(m.Ascent.Ceil()), float64(m.Descent.Ceil())
	c.text(f, s, (r.x0+r.x1-w)/2, (r.y0+r.y1+asc-desc)/2, col)
}

func (c *canvas) boxText(f font.Face, s string, r rect, bg, border, fg color.Color) {
	c.fill([][]fpt{{{r.x0, r.y0}, {r.x1, r.y0}, {r.x1, r.y1}, {r.x0, r.y1}}}, bg)
	c.stroke([][]fpt{{{r.x0, r.y0}, {r.x1, r.y0}, {r.x1, r.y1}, {r.x0, r.y1}, {r.x0, r.y0}}}, 1, border)
	c.textIn(f, s, r, fg)
}
