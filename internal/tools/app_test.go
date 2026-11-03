package tools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/kensantoso/snag-mcp/internal/storemap"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// appSession connects a client to the tools, declaring MCP Apps or not, on
// the given protocol version ("" is the SDK's latest, which carries the
// client's capabilities in each request's _meta rather than in initialize).
func appSession(t *testing.T, d *Deps, ui bool, version string) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	Register(srv, d)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	caps := &mcp.ClientCapabilities{}
	if ui {
		caps.AddExtension(uiExtension, map[string]any{"mimeTypes": []string{uiMIMEType}})
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, &mcp.ClientOptions{Capabilities: caps}).
		Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func newDeps(t *testing.T) *Deps {
	return &Deps{API: bunnings.New(1), ConfigPath: filepath.Join(t.TempDir(), "config.json")}
}

func uiOf(t *testing.T, m mcp.Meta) map[string]any {
	t.Helper()
	b, _ := json.Marshal(m)
	var v struct {
		UI map[string]any `json:"ui"`
	}
	json.Unmarshal(b, &v)
	return v.UI
}

// A host that declares MCP Apps, on either protocol generation, sees
// check_list linked to the view and the app-only layout tool marked as such;
// one that does not never sees the layout tool, so its model is not offered
// a tool it has no use for.
func TestAppNegotiation(t *testing.T) {
	for _, version := range []string{"", "2025-11-25"} {
		for _, ui := range []bool{true, false} {
			cs := appSession(t, newDeps(t), ui, version)
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var check, layout *mcp.Tool
			for _, tool := range res.Tools {
				switch tool.Name {
				case "check_list":
					check = tool
				case layoutTool:
					layout = tool
				}
			}
			name := cs.InitializeResult().ProtocolVersion
			if check == nil || uiOf(t, check.Meta)["resourceUri"] != pickListURI || uiOf(t, check.Meta)["visibility"] != nil {
				t.Errorf("%s ui=%v: check_list _meta %v", name, ui, check)
			}
			if !ui {
				if layout != nil {
					t.Errorf("%s: %s listed for a client without MCP Apps", name, layoutTool)
				}
				continue
			}
			if layout == nil {
				t.Fatalf("%s: %s not listed for a client with MCP Apps", name, layoutTool)
			}
			if v, _ := uiOf(t, layout.Meta)["visibility"].([]any); !slices.Equal(v, []any{"app"}) {
				t.Errorf("%s: %s visibility %v", name, layoutTool, v)
			}
		}
	}
}

// The view is one self-contained page served exactly as the spec requires,
// loading nothing from anywhere, which the host's default CSP would block.
func TestAppResource(t *testing.T) {
	cs := appSession(t, newDeps(t), true, "")
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: pickListURI})
	if err != nil || len(res.Contents) != 1 {
		t.Fatalf("read: %v %+v", err, res)
	}
	c := res.Contents[0]
	if c.URI != pickListURI || c.MIMEType != "text/html;profile=mcp-app" || !strings.HasPrefix(c.Text, "<!doctype html>") {
		t.Fatalf("contents: %s %s %.40q", c.URI, c.MIMEType, c.Text)
	}
	if ui := uiOf(t, c.Meta); ui["csp"] != nil || ui["prefersBorder"] != true {
		t.Errorf("_meta.ui = %v", ui)
	}
	// No external scripts, styles, images, fonts or connections.
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//|@import|url\(\s*["']?https?:|fetch\(|XMLHttpRequest|WebSocket`).FindString(c.Text); m != "" {
		t.Errorf("view reaches out: %q", m)
	}
	for _, method := range []string{`"ui/initialize"`, `"ui/notifications/initialized"`, `"ui/notifications/tool-result"`, `"tools/call"`, `"get_store_layout"`, "Unofficial; not affiliated with Bunnings."} {
		if !strings.Contains(c.Text, method) {
			t.Errorf("view lacks %s", method)
		}
	}
}

// preloaded is Deps with Example Store's plan already loaded, as if check_list had
// just fetched it, so the layout tool needs no network.
func preloaded(t *testing.T) (*Deps, *loadedMap) {
	d := newDeps(t)
	lm := exampleStore(t)
	d.maps = map[string]cached[*loadedMap]{"0001": {lm, time.Now()}}
	return d, lm
}

func callLayout(t *testing.T, cs *mcp.ClientSession, args map[string]any) (layoutOut, *mcp.CallToolResult) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: layoutTool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out layoutOut
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	return out, res
}

// The layout shares check_list's origin and units (decimetres to its metres),
// and its route is the walk check_list ordered the stops by.
func TestStoreLayout(t *testing.T) {
	d, lm := preloaded(t)
	lines := []shop.Line{line("1", shop.InStock, "10", "3"), line("2", shop.InStock, "2", "1"), line("3", shop.InStock, "21", "1")}
	w, off := walkLines(lines, lm)
	sm := mapOut(lm, w, lines, off)
	var stops []string
	for _, s := range sm.Stops {
		stops = append(stops, s.Address)
	}
	out, res := callLayout(t, appSession(t, d, true, ""), map[string]any{"store_id": "0001", "map_id": lm.Map.ID, "stops": stops})
	if res.IsError {
		t.Fatalf("error: %v", res.Content)
	}
	if out.MapID != lm.Map.ID || len(out.Storeys) != 2 || out.Ordering != storemap.ByWalkingGraph || out.Bytes == 0 || out.Bytes > maxLayoutBytes {
		t.Fatalf("map %s, %d storeys, ordering %s, %d bytes", out.MapID, len(out.Storeys), out.Ordering, out.Bytes)
	}
	near := func(dm int, m float64) bool { return math.Abs(float64(dm)-m*10) <= 1 }
	if out.Start == nil || !near(toInt((*out.Start)[1]), sm.Start.X) || !near(toInt((*out.Start)[2]), sm.Start.Y) || toInt((*out.Start)[3]) != sm.Start.Storey {
		t.Errorf("start %v, check_list start %+v", out.Start, sm.Start)
	}
	// One leg into each stop and one to the registers, each ending at its stop.
	if len(out.Route) != len(sm.Stops)+1 {
		t.Fatalf("%d legs for %d stops", len(out.Route), len(sm.Stops))
	}
	var walked float64
	floors := map[int]bool{}
	for i, leg := range out.Route {
		for k := 1; k < len(leg); k++ {
			floors[leg[k][0]] = true
			if leg[k][0] == leg[k-1][0] {
				walked += math.Hypot(float64(leg[k][1]-leg[k-1][1]), float64(leg[k][2]-leg[k-1][2])) / 10
			}
		}
		if i < len(sm.Stops) {
			end, s := leg[len(leg)-1], sm.Stops[i]
			if end[0] != s.Storey || math.Hypot(float64(end[1])/10-s.X, float64(end[2])/10-s.Y) > 3 {
				t.Errorf("leg %d ends at %v, stop %d is at %.1f,%.1f on %d", i, end, s.Stop, s.X, s.Y, s.Storey)
			}
		}
	}
	if math.Abs(walked-*sm.WalkM) > 0.02**sm.WalkM || !floors[1] || !floors[2] {
		t.Errorf("route %.1f m on floors %v; check_list walk %.1f m", walked, floors, *sm.WalkM)
	}
	// Each stop's bay is one of the layout's bays, in the same place.
	for _, st := range sm.Stops {
		want := make([]int, 0, 8)
		for _, p := range st.Bay2D[:4] {
			want = append(want, int(math.Round(p[0]*10)), int(math.Round(p[1]*10)))
		}
		found := false
		for _, b := range out.Storeys[st.Storey-1].Bays {
			if len(b) >= 8 && slices.EqualFunc(b[:8], want, func(a, b int) bool { return a-b <= 1 && b-a <= 1 }) {
				found = true
			}
		}
		if !found {
			t.Errorf("stop %d's bay %v is not among the layout's bays", st.Stop, want)
		}
	}
	ground := out.Storeys[1]
	if ground.Name != "Ground" || len(ground.Bays) == 0 || len(ground.Aisles) == 0 || len(ground.Entrances) == 0 || len(ground.Outline) < 6 || ground.Outline[0] < 0 {
		t.Errorf("ground storey %s: %d bays, %d aisles, %d entrances, outline %v", ground.Name, len(ground.Bays), len(ground.Aisles), len(ground.Entrances), ground.Outline[:6])
	}
	// Without stops it is the plan alone.
	out, _ = callLayout(t, appSession(t, d, true, ""), map[string]any{"store_id": "0001", "map_id": lm.Map.ID})
	if len(out.Storeys) != 2 || out.Route != nil || out.Start != nil {
		t.Errorf("no stops: %d storeys, route %v, start %v", len(out.Storeys), out.Route, out.Start)
	}
}

func toInt(v any) int { f, _ := v.(float64); return int(f) }

// The view passes back what check_list gave it; anything else is refused
// before a request goes anywhere.
func TestStoreLayoutRejects(t *testing.T) {
	d, lm := preloaded(t)
	cs := appSession(t, d, true, "")
	many := make([]any, maxListItems+1)
	for i := range many {
		many[i] = "010.003"
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"store_id": "74", "map_id": "0001-0001"}, "four digits"},
		{map[string]any{"store_id": "0001/../x", "map_id": "0001-0001"}, "four digits"},
		{map[string]any{"store_id": "0001", "map_id": "0001-1"}, "map id"},
		{map[string]any{"store_id": "0001", "map_id": "1234-0001"}, "map id"},
		{map[string]any{"store_id": "0001", "map_id": lm.Map.ID, "stops": []any{"10.3"}}, "bay address"},
		{map[string]any{"store_id": "0001", "map_id": lm.Map.ID, "stops": []any{"010.003; DROP"}}, "bay address"},
		{map[string]any{"store_id": "0001", "map_id": lm.Map.ID, "stops": many}, "at most"},
		{map[string]any{"store_id": "0001", "map_id": lm.Map.ID, "stops": []any{"999.999"}}, "not on store"},
		{map[string]any{"store_id": "0001", "map_id": "0001-9999"}, "run check_list again"},
	} {
		_, res := callLayout(t, cs, c.args)
		if !res.IsError || !strings.Contains(textOfResult(res), c.want) {
			t.Errorf("%v: error %v, %q; want %q", c.args, res.IsError, textOfResult(res), c.want)
		}
	}
}

func textOfResult(r *mcp.CallToolResult) string {
	var s []string
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			s = append(s, t.Text)
		}
	}
	return strings.Join(s, " ")
}

// A plan too big to be a store is refused rather than sent.
func TestStoreLayoutSizeBound(t *testing.T) {
	pt := func(x, y float64) storemap.Point { return storemap.Point{X: x, Y: y} }
	s := &storemap.Storey{Number: 1, Name: "Ground", Max: pt(1e6, 1e6),
		Outline: []storemap.Point{pt(0, 0), pt(1e6, 0), pt(1e6, 1e6), pt(0, 1e6)}}
	for i := range 60000 {
		x, y := float64(i%250)*4000, float64(i/250)*4000
		s.Bays = append(s.Bays, storemap.Bay{Addr: storemap.Addr{Aisle: i/20 + 1, Bay: i%20 + 1},
			Poly: []storemap.Point{pt(x, y), pt(x+3000, y), pt(x+3000, y+1000), pt(x, y+1000)}})
	}
	_, err := buildLayout(&loadedMap{Map: &storemap.Map{ID: "9999-0001", Store: "9999", Storeys: []*storemap.Storey{s}}}, nil)
	if !errors.Is(err, errLayoutTooBig) {
		t.Fatalf("err = %v", err)
	}
	// Example Store, a real store, is well inside it.
	lm := exampleStore(t)
	out, err := buildLayout(lm, nil)
	if err != nil || out.Bytes > maxLayoutBytes/8 {
		t.Errorf("Example Store: %v, %d bytes", err, out.Bytes)
	}
}
