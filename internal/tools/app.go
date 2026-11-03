package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"

	"github.com/kensantoso/snag-mcp/internal/storemap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The pick list as an MCP App (SEP-1865): hosts that support the extension
// show check_list's pick list and store map inline, drawn from data, instead
// of leaving the PNG inside the collapsed tool result. Everything a host
// without the extension gets is unchanged.

const (
	// uiExtension is the MCP Apps extension id.
	uiExtension = "io.modelcontextprotocol/ui"
	uiMIMEType  = "text/html;profile=mcp-app"
	pickListURI = "ui://snag/pick-list"
	layoutTool  = "get_store_layout"
)

// pickListHTML is the view: one self-contained page, no network access.
//
//go:embed app/pick-list.html
var pickListHTML string

// uiMeta links a tool to the view, for hosts that render MCP Apps.
func uiMeta(visibility ...string) mcp.Meta {
	ui := map[string]any{"resourceUri": pickListURI}
	if len(visibility) > 0 {
		ui["visibility"] = visibility
	}
	return mcp.Meta{"ui": ui}
}

func registerApp(s *mcp.Server, d *Deps) {
	s.AddResource(&mcp.Resource{
		URI:         pickListURI,
		Name:        "pick_list",
		Title:       "Pick list and store map",
		Description: "check_list's pick list for one store, with the store's floor plan, the walking route and each stop numbered.",
		MIMEType:    uiMIMEType,
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: pickListURI, MIMEType: uiMIMEType, Text: pickListHTML,
			// No csp: the view loads nothing and connects nowhere; its data
			// comes from the host. The restrictive default is what it wants.
			Meta: mcp.Meta{"ui": map[string]any{"prefersBorder": true}},
		}}}, nil
	})
	registerStoreLayout(s, d)
	s.AddReceivingMiddleware(hideAppTools)
}

// uiSupported reports whether the client declared the MCP Apps extension,
// and its settings. The SDK reads the declaration from the request's _meta
// on the 2026-07-28 protocol and from initialize before it.
func uiSupported(caps *mcp.ClientCapabilities) (any, bool) {
	if caps == nil {
		return nil, false
	}
	v, ok := caps.Extensions[uiExtension]
	if !ok {
		return nil, false
	}
	m, _ := v.(map[string]any)
	types, _ := m["mimeTypes"].([]any)
	return v, slices.Contains(types, any(uiMIMEType))
}

// hideAppTools keeps app-only tools out of the tool list of a client that did
// not declare MCP Apps. Such a client would not know to hide them from its
// model, which would then see a tool it has no use for. It also logs, once a
// listing, whether the client declared the extension, which is how to tell
// that a host negotiated it.
func hideAppTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		lr, ok := req.(*mcp.ListToolsRequest)
		if err != nil || !ok {
			return res, err
		}
		client := "client"
		if ci := lr.ClientInfo(); ci != nil {
			client = fmt.Sprintf("client %q %s", ci.Name, ci.Version)
		}
		caps := lr.ClientCapabilities()
		if settings, yes := uiSupported(caps); yes {
			b, _ := json.Marshal(settings)
			fmt.Fprintf(os.Stderr, "snag-mcp: %s declares %s %s: check_list shows the pick-list app\n", client, uiExtension, b)
			return res, nil
		}
		fmt.Fprintf(os.Stderr, "snag-mcp: %s does not declare %s with %s: plain results, %s hidden\n", client, uiExtension, uiMIMEType, layoutTool)
		if tr, ok := res.(*mcp.ListToolsResult); ok {
			cp := *tr
			cp.Tools = slices.DeleteFunc(slices.Clone(tr.Tools), func(t *mcp.Tool) bool { return t.Name == layoutTool })
			return &cp, nil
		}
		return res, nil
	}
}

// ---------- get_store_layout ----------

type layoutIn struct {
	StoreID string   `json:"store_id" jsonschema:"Store id, four digits, from check_list's pick_list.store.store_id"`
	MapID   string   `json:"map_id" jsonschema:"Map id from check_list's store_map.map_id, e.g. 6400-0017"`
	Stops   []string `json:"stops,omitempty" jsonschema:"The stops' bay addresses in walking order, as store_map.stops[].address gives them (e.g. 026.009), for the walking route between them"`
}

// layoutOut is a store's floor plan, simplified for drawing. Coordinates are
// whole decimetres from the top-left corner of each storey's outline, the
// same origin as check_list's x_m and y_m, so a stop at x_m 13.3 is at 133.
type layoutOut struct {
	StoreID   string         `json:"store_id"`
	StoreName string         `json:"store_name"`
	MapID     string         `json:"map_id"`
	Units     string         `json:"units"`
	Storeys   []layoutStorey `json:"storeys"`
	Ordering  string         `json:"ordering,omitempty"`
	Route     []layoutLeg    `json:"route,omitempty"`
	Start     *layoutLabel   `json:"start,omitempty"`
	End       *layoutLabel   `json:"end,omitempty"`
	RouteNote string         `json:"route_note,omitempty"`
	Bytes     int            `json:"bytes"`
}

// layoutLabel is [name, x, y] (and storey, for start and end).
type layoutLabel []any

// layoutArea is [name, [x0, y0, x1, y1, ...]].
type layoutArea []any

// layoutLeg is one leg of the route as [storey, x, y] points.
type layoutLeg [][3]int

type layoutStorey struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	// Outline and each bay are flat [x0, y0, x1, y1, ...] polygons.
	Outline     []int         `json:"outline"`
	Bays        [][]int       `json:"bays"`
	Promo       [][]int       `json:"promo_bays"`
	Aisles      []layoutLabel `json:"aisles"`
	Departments []layoutLabel `json:"departments"`
	Entrances   []layoutLabel `json:"entrances"`
	Landmarks   []layoutArea  `json:"landmarks"`
	LevelAccess []layoutArea  `json:"level_access"`
}

// maxLayoutBytes bounds what one layout sends to the view. Mentone, with
// 1,672 bays, is about 75 KB; a map ten times that is not a store plan.
const maxLayoutBytes = 1 << 20

var (
	storeIDRE = regexp.MustCompile(`^[0-9]{4}$`)
	mapIDRE   = regexp.MustCompile(`^[0-9]{4}-[0-9]{4}$`)
	addrRE    = regexp.MustCompile(`^[0-9]{3,4}\.[0-9]{3,4}$`)
)

func registerStoreLayout(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: layoutTool,
		Description: "For the pick-list view only: a store's floor plan, simplified for drawing, and the walking route " +
			"between the given stops. Not for answering questions; check_list has everything to tell the user.",
		Meta: uiMeta("app"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in layoutIn) (*mcp.CallToolResult, layoutOut, error) {
		if err := validLayoutIn(in); err != nil {
			return nil, layoutOut{}, err
		}
		lm, err := d.storeMap(ctx, in.StoreID)
		if err != nil {
			return nil, layoutOut{}, fmt.Errorf("no floor plan for store %s: %w", in.StoreID, err)
		}
		if lm.Map.ID != in.MapID {
			return nil, layoutOut{}, fmt.Errorf("store %s's plan is now %s, not %s: run check_list again", in.StoreID, lm.Map.ID, in.MapID)
		}
		out, err := buildLayout(lm, in.Stops)
		if err != nil {
			return nil, layoutOut{}, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: fmt.Sprintf("Floor plan %s for %s: %d storeys, %d bytes.", out.MapID, out.StoreName, len(out.Storeys), out.Bytes),
		}}}, out, nil
	})
}

func validLayoutIn(in layoutIn) error {
	switch {
	case !storeIDRE.MatchString(in.StoreID):
		return fmt.Errorf("store_id must be four digits, as check_list gives it")
	case !mapIDRE.MatchString(in.MapID) || in.MapID[:4] != in.StoreID:
		return fmt.Errorf("map_id must be the store's map id from check_list, e.g. %s-0017", in.StoreID)
	case len(in.Stops) > maxListItems:
		return fmt.Errorf("at most %d stops", maxListItems)
	}
	for _, a := range in.Stops {
		if !addrRE.MatchString(a) {
			return fmt.Errorf("stop %q is not a bay address like 026.009", a)
		}
	}
	return nil
}

// decimetres is p in whole decimetres from the top-left of s's outline.
func decimetres(s *storemap.Storey, p storemap.Point) (int, int) {
	return int(math.Round((p.X - s.Min.X) / 100)), int(math.Round((p.Y - s.Min.Y) / 100))
}

var errLayoutTooBig = errors.New("the store's floor plan is too large to send")

func buildLayout(lm *loadedMap, stops []string) (layoutOut, error) {
	m := lm.Map
	out := layoutOut{StoreID: m.Store, StoreName: m.StoreName, MapID: m.ID,
		Units: "decimetres from the top-left corner of each storey's outline, y down"}
	for _, s := range m.Storeys {
		dm := func(p storemap.Point) (int, int) { return decimetres(s, p) }
		flat := func(ps []storemap.Point) []int {
			out := make([]int, 0, 2*len(ps))
			for i, p := range ps {
				// Polygons come closed; the view closes them itself.
				if i == len(ps)-1 && len(ps) > 3 && p == ps[0] {
					break
				}
				x, y := dm(p)
				out = append(out, x, y)
			}
			return out
		}
		lbl := func(ls []storemap.Label) []layoutLabel {
			out := make([]layoutLabel, 0, len(ls))
			for _, l := range ls {
				x, y := dm(l.At)
				out = append(out, layoutLabel{l.Name, x, y})
			}
			return out
		}
		ar := func(as []storemap.Area) []layoutArea {
			out := make([]layoutArea, 0, len(as))
			for _, a := range as {
				out = append(out, layoutArea{a.Name, flat(a.Poly)})
			}
			return out
		}
		w, h := dm(s.Max)
		ls := layoutStorey{Number: s.Number, Name: s.Name, W: w, H: h, Outline: flat(s.Outline),
			Bays: [][]int{}, Promo: [][]int{},
			Aisles: lbl(s.Aisles), Departments: lbl(s.Departments), Entrances: lbl(s.Entrances),
			Landmarks: ar(s.Landmarks), LevelAccess: ar(s.LevelAccess)}
		for _, b := range s.Bays {
			if b.Promo {
				ls.Promo = append(ls.Promo, flat(b.Poly))
			} else {
				ls.Bays = append(ls.Bays, flat(b.Poly))
			}
		}
		out.Storeys = append(out.Storeys, ls)
	}

	if len(stops) > 0 {
		var bays []storemap.Located
		for _, a := range stops {
			var aisle, bay int
			fmt.Sscanf(a, "%d.%d", &aisle, &bay)
			loc, ok := m.Find(storemap.Addr{Aisle: aisle, Bay: bay})
			if !ok {
				return layoutOut{}, fmt.Errorf("bay %s is not on store %s's plan", a, m.Store)
			}
			bays = append(bays, loc)
		}
		w := storemap.Retrace(m, lm.Graph, bays)
		out.Ordering = w.Method
		if why := straightLineWhy(lm, w); why != "" {
			out.RouteNote = "No walking route: " + why + "."
		}
		for _, leg := range w.Legs {
			var l layoutLeg
			for _, p := range leg {
				s := m.Storey(p.Storey)
				if s == nil {
					continue
				}
				x, y := decimetres(s, p.Point)
				l = append(l, [3]int{p.Storey, x, y})
			}
			out.Route = append(out.Route, l)
		}
		place := func(storey int, l *storemap.Label) *layoutLabel {
			s := m.Storey(storey)
			if l == nil || s == nil {
				return nil
			}
			x, y := decimetres(s, l.At)
			v := layoutLabel{l.Name, x, y, storey}
			return &v
		}
		out.Start, out.End = place(w.StartStorey, w.Start), place(w.StartStorey, w.End)
	}

	b, err := json.Marshal(out)
	if err != nil {
		return layoutOut{}, err
	}
	if len(b) > maxLayoutBytes {
		return layoutOut{}, fmt.Errorf("%w (%d KB)", errLayoutTooBig, len(b)>>10)
	}
	out.Bytes = len(b)
	return out, nil
}
