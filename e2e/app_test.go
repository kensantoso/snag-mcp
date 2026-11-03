package e2e

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startUI launches the binary for a client that declares MCP Apps, as Claude
// Desktop and other app-rendering hosts do.
func startUI(t *testing.T) *server {
	t.Helper()
	caps := &mcp.ClientCapabilities{}
	caps.AddExtension("io.modelcontextprotocol/ui", map[string]any{"mimeTypes": []string{"text/html;profile=mcp-app"}})
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-apps", Version: "1"}, &mcp.ClientOptions{Capabilities: caps})
	cmd := exec.Command(binary, "-config", filepath.Join(t.TempDir(), "config.json"))
	cmd.Stderr = os.Stderr
	s, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &server{s: s}
}

func toolNames(t *testing.T, srv *server) []string {
	t.Helper()
	res, err := srv.s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func contentTypes(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		b, _ := json.Marshal(c)
		var v struct{ Type string }
		json.Unmarshal(b, &v)
		out = append(out, v.Type)
	}
	return out
}

// The pick-list app end to end: a host that declares MCP Apps gets the same
// check_list result as one that does not, and the view's layout call returns
// a plan whose bays are the stops' bays and a route with a leg into each stop.
func TestLiveApp(t *testing.T) {
	plain, ui := start(t, ""), startUI(t)
	if slices.Contains(toolNames(t, plain), "get_store_layout") || !slices.Contains(toolNames(t, ui), "get_store_layout") {
		t.Fatalf("tools: plain %v, apps %v", toolNames(t, plain), toolNames(t, ui))
	}
	for _, c := range []struct {
		store, location string
		items           []string
		storeys         int
	}{
		{"6400", "3194", []string{"1660009", "0173348", "0730103", "1370215", "0363419"}, 1},
		{"6299", "3122", []string{"0563516", "3129102", "0548292", "8032174"}, 3},
	} {
		var items []map[string]any
		for _, it := range c.items {
			items = append(items, map[string]any{"item_number": it})
		}
		args := map[string]any{"items": items, "store": c.store, "location": c.location}
		a, err := plain.callRaw("check_list", args)
		if err != nil || a.IsError {
			t.Fatalf("%s: %v %s", c.store, err, text(a))
		}
		b, err := ui.callRaw("check_list", args)
		if err != nil || b.IsError {
			t.Fatalf("%s: %v %s", c.store, err, text(b))
		}
		if !slices.Equal(contentTypes(a), contentTypes(b)) || !slices.Equal(contentTypes(b), []string{"text", "image", "text"}) {
			t.Errorf("%s: content %v without apps, %v with", c.store, contentTypes(a), contentTypes(b))
		}
		var cl struct {
			StoreMap struct {
				MapID string `json:"map_id"`
				Stops []struct {
					Stop    int          `json:"stop"`
					Address string       `json:"address"`
					Storey  int          `json:"storey"`
					X       float64      `json:"x_m"`
					Y       float64      `json:"y_m"`
					Bay     [][2]float64 `json:"bay_outline"`
				} `json:"stops"`
			} `json:"store_map"`
		}
		raw, _ := json.Marshal(b.StructuredContent)
		json.Unmarshal(raw, &cl)
		var stops []string
		for _, s := range cl.StoreMap.Stops {
			stops = append(stops, s.Address)
		}
		var lay struct {
			MapID   string `json:"map_id"`
			Bytes   int    `json:"bytes"`
			Storeys []struct {
				Number int     `json:"number"`
				Bays   [][]int `json:"bays"`
				Promo  [][]int `json:"promo_bays"`
			} `json:"storeys"`
			Route [][][3]int `json:"route"`
		}
		ui.call(t, "get_store_layout", map[string]any{"store_id": c.store, "map_id": cl.StoreMap.MapID, "stops": stops}, &lay)
		t.Logf("%s: map %s, %d storeys, %d bytes, %d stops, %d route legs", c.store, lay.MapID, len(lay.Storeys), lay.Bytes, len(stops), len(lay.Route))
		if lay.MapID != cl.StoreMap.MapID || len(lay.Storeys) != c.storeys || len(lay.Route) != len(stops)+1 || len(stops) == 0 {
			t.Fatalf("%s: layout %s with %d storeys and %d legs for %d stops", c.store, lay.MapID, len(lay.Storeys), len(lay.Route), len(stops))
		}
		for i, s := range cl.StoreMap.Stops {
			end := lay.Route[i][len(lay.Route[i])-1]
			if end[0] != s.Storey || math.Hypot(float64(end[1])/10-s.X, float64(end[2])/10-s.Y) > 3 {
				t.Errorf("%s stop %d: route leg ends at %v, stop at %.1f,%.1f on %d", c.store, s.Stop, end, s.X, s.Y, s.Storey)
			}
			found := false
			for _, st := range lay.Storeys {
				if st.Number != s.Storey {
					continue
				}
				for _, bay := range append(slices.Clone(st.Bays), st.Promo...) {
					if len(bay) >= 2 && math.Abs(float64(bay[0])/10-s.Bay[0][0]) <= 0.1 && math.Abs(float64(bay[1])/10-s.Bay[0][1]) <= 0.1 {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("%s stop %d (%s): its bay is not in the layout", c.store, s.Stop, s.Address)
			}
		}
	}
}
