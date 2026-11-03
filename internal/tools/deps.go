// Package tools exposes Bunnings product data as MCP tools.
//
// Every tool answers for a place and a radius, and says so: which location,
// how far, and how many stores the answer covers. An item appears only if a
// store inside that radius sells it in person, so the assistant cannot suggest
// something the user would have to have shipped from Perth.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/kensantoso/snag-mcp/internal/geo"
	"github.com/kensantoso/snag-mcp/internal/shop"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/singleflight"
)

const (
	defaultRadiusKM = 50
	maxRadiusKM     = 500
	// maxSearchStores caps the per-store fields one search requests. Fifty
	// stores within 50 km of the Melbourne CBD is typical; a wider radius in a
	// capital would otherwise ask for hundreds of fields.
	maxSearchStores = 60
)

// Deps are what the tools operate on.
type Deps struct {
	API        *bunnings.Client
	ConfigPath string // where the user's location is remembered

	mu       sync.Mutex
	stores   map[string]cached[[]bunnings.Store]
	products map[string]cached[*bunnings.Product]
	brands   map[string]cached[[]string]
	maps     map[string]cached[*loadedMap]
	// mapLoads joins concurrent loads of one store's plan, which is several
	// megabytes, into one.
	mapLoads singleflight.Group
}

type cached[T any] struct {
	v  T
	at time.Time
}

// Store lists and product details change rarely; stock and price are fetched
// fresh every time.
const cacheTTL = time.Hour

// Register adds every tool to the server.
func Register(s *mcp.Server, d *Deps) {
	registerSetLocation(s, d)
	registerSearchProducts(s, d)
	registerGetProduct(s, d)
	registerCheckList(s, d)
	registerGetReviews(s, d)
	registerApp(s, d)
}

// ---------- location ----------

type config struct {
	Location *geo.Place `json:"location,omitempty"`
}

// DefaultConfigPath is where the location is remembered between sessions.
func DefaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "snag-mcp", "config.json")
}

func (d *Deps) savedLocation() *geo.Place {
	b, err := os.ReadFile(d.ConfigPath)
	if err != nil {
		return nil
	}
	var c config
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return c.Location
}

// SaveLocation remembers p as the user's location.
func (d *Deps) SaveLocation(p geo.Place) error {
	if err := os.MkdirAll(filepath.Dir(d.ConfigPath), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(config{Location: &p}, "", "  ")
	// Owner-only: where someone lives is nobody else's business.
	return os.WriteFile(d.ConfigPath, append(b, '\n'), 0o600)
}

var errNoLocation = errors.New("the user's location is not set. Ask them for their postcode or suburb " +
	"(do not guess it or infer it from anything else), then call set_location with it, then retry")

// errAskUser is returned by where when the client can be asked for the
// location; addTool turns it into an input request rather than an error.
var errAskUser = errors.New("location needed")

const locationInput = "location"

var askLocation = &mcp.ElicitParams{
	Message: "Which postcode or suburb should I find Bunnings stores near? It is saved on this computer for next time.",
	RequestedSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{
				"type":        "string",
				"title":       "Postcode or suburb",
				"description": "e.g. 3194 or Mentone VIC",
			},
		},
		"required": []string{"location"},
	},
}

// where resolves the location for a call: the override if given, else the
// remembered one, else the user is asked through elicitation. A client that
// cannot elicit, or a user who declines, gets an error telling the assistant
// to ask instead. The location is never inferred from the network.
func (d *Deps) where(ctx context.Context, req *mcp.CallToolRequest, override string) (geo.Place, error) {
	if override != "" {
		return geo.Resolve(override)
	}
	if p := d.savedLocation(); p != nil {
		return *p, nil
	}
	if req == nil || req.Session == nil || req.Params == nil {
		return geo.Place{}, errNoLocation
	}
	// The answer to an earlier ask, on the retried call.
	if r, ok := req.Params.InputResponses[locationInput].(*mcp.ElicitResult); ok {
		s, _ := r.Content["location"].(string)
		if r.Action != "accept" || strings.TrimSpace(s) == "" {
			return geo.Place{}, errNoLocation
		}
		p, err := geo.Resolve(s)
		if err != nil {
			return geo.Place{}, fmt.Errorf("%w. Ask the user again and call set_location", err)
		}
		if err := d.SaveLocation(p); err != nil {
			return geo.Place{}, err
		}
		return p, nil
	}
	if ip := req.Session.InitializeParams(); ip == nil || ip.Capabilities == nil || ip.Capabilities.Elicitation == nil {
		return geo.Place{}, errNoLocation
	}
	return geo.Place{}, errAskUser
}

// addTool registers a tool whose handler may need to ask the user for their
// location. It is asked as an input request: clients on the current protocol
// answer it and retry the call, and the SDK bridges older clients with a
// direct elicitation, so one path serves both.
func addTool[In, Out any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		res, out, err := h(ctx, req, in)
		if errors.Is(err, errAskUser) {
			var zero Out
			return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{locationInput: askLocation}}, zero, nil
		}
		return res, out, err
	})
}

// ---------- stores ----------

func (d *Deps) storesNear(ctx context.Context, lat, lon, radiusKM float64, max int) ([]bunnings.Store, error) {
	key := fmt.Sprintf("%.3f,%.3f,%.0f,%d", lat, lon, radiusKM, max)
	d.mu.Lock()
	if c, ok := d.stores[key]; ok && time.Since(c.at) < cacheTTL {
		d.mu.Unlock()
		return c.v, nil
	}
	d.mu.Unlock()
	st, err := d.API.StoresNear(ctx, lat, lon, radiusKM, max)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.stores == nil {
		d.stores = map[string]cached[[]bunnings.Store]{}
	}
	d.stores[key] = cached[[]bunnings.Store]{st, time.Now()}
	d.mu.Unlock()
	return st, nil
}

// Coverage says what an answer covers. It is on every result so a reply can
// never quietly be about fewer stores than it appears to be.
type Coverage struct {
	Location       string  `json:"location"`
	RadiusKM       float64 `json:"radius_km"`
	StoresInRadius int     `json:"stores_in_radius"`
	StoresCovered  int     `json:"stores_covered"`
	// NearestOutside is set only when no store is inside the radius.
	NearestOutside *shop.Store `json:"nearest_store_outside_radius,omitempty"`
	Note           string      `json:"note,omitempty"`
}

type area struct {
	place  geo.Place
	radius float64
	stores []shop.Store // within radius, nearest first
	region string       // pricing region of the nearest store
	cov    Coverage
}

func radiusOrDefault(r float64) (float64, error) {
	switch {
	case r == 0:
		return defaultRadiusKM, nil
	case r < 0 || r > maxRadiusKM:
		return 0, fmt.Errorf("radius_km must be between 1 and %d", maxRadiusKM)
	}
	return r, nil
}

// nearby finds the stores within radius of the call's location.
//
// The gateway's own radius is loose, so distance is measured here, from the
// same point, and the boundary is ours. When nothing is in range the nearest
// store is named, so the assistant can ask before widening.
func (d *Deps) nearby(ctx context.Context, req *mcp.CallToolRequest, location string, radiusKM float64) (*area, error) {
	place, err := d.where(ctx, req, location)
	if err != nil {
		return nil, err
	}
	return d.around(ctx, place, radiusKM)
}

func (d *Deps) around(ctx context.Context, place geo.Place, radiusKM float64) (*area, error) {
	radius, err := radiusOrDefault(radiusKM)
	if err != nil {
		return nil, err
	}
	raw, err := d.storesNear(ctx, place.Lat, place.Lon, radius+10, 150)
	if err != nil {
		return nil, err
	}
	a := &area{place: place, radius: radius}
	a.cov = Coverage{Location: place.Label(), RadiusKM: radius}
	for _, s := range raw {
		km := round1(geo.DistanceKM(place.Lat, place.Lon, s.Lat, s.Lon))
		if km > radius {
			continue
		}
		if a.region == "" {
			a.region = s.PricingRegion
		}
		a.stores = append(a.stores, shop.Store{ID: s.ID, Name: s.Name, State: s.State, DistanceKM: km})
	}
	a.cov.StoresInRadius = len(a.stores)
	if len(a.stores) == 0 {
		// A continent-sized radius with one result is the nearest store.
		if far, err := d.storesNear(ctx, place.Lat, place.Lon, 5000, 1); err == nil && len(far) > 0 {
			s := far[0]
			a.cov.NearestOutside = &shop.Store{ID: s.ID, Name: s.Name, State: s.State,
				DistanceKM: round1(geo.DistanceKM(place.Lat, place.Lon, s.Lat, s.Lon))}
		}
		a.cov.Note = fmt.Sprintf("No Bunnings store within %.0f km of %s.", radius, place.Label())
		if n := a.cov.NearestOutside; n != nil {
			a.cov.Note += fmt.Sprintf(" The nearest is %s, %.0f km away. Ask the user before searching with a wider radius_km.", n.Name, n.DistanceKM)
		}
	}
	return a, nil
}

// searchStores is the subset of stores a search requests fields for.
func (a *area) searchStores() []shop.Store {
	if len(a.stores) > maxSearchStores {
		return a.stores[:maxSearchStores]
	}
	return a.stores
}

// brandsNear lists every brand with an item that some of the stores might
// sell in person, from one facet request (about 1,500 brands around
// Melbourne). Cached, since it changes no faster than the range does.
func (d *Deps) brandsNear(ctx context.Context, stores []shop.Store, region string) ([]string, error) {
	ids := make([]string, len(stores))
	for i, s := range stores {
		ids[i] = s.ID
	}
	key := strings.Join(ids, ",")
	d.mu.Lock()
	if c, ok := d.brands[key]; ok && time.Since(c.at) < cacheTTL {
		d.mu.Unlock()
		return c.v, nil
	}
	d.mu.Unlock()
	res, err := d.API.Search(ctx, bunnings.SearchRequest{
		AQ: shop.AvailabilityQuery(stores), Store: stores[0].ID, Region: region,
		Count: 0, Facet: "@brandname", FacetMax: 5000,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(res.Facet))
	for i, f := range res.Facet {
		out[i] = f.Value
	}
	d.mu.Lock()
	if d.brands == nil {
		d.brands = map[string]cached[[]string]{}
	}
	d.brands[key] = cached[[]string]{out, time.Now()}
	d.mu.Unlock()
	return out, nil
}

// ---------- products ----------

// product fetches details as seen from a store (the nearest, normally), which
// is what makes its special-order flag mean something.
func (d *Deps) product(ctx context.Context, item, store string) (*bunnings.Product, error) {
	key := item + "@" + store
	d.mu.Lock()
	if c, ok := d.products[key]; ok && time.Since(c.at) < cacheTTL {
		d.mu.Unlock()
		return c.v, nil
	}
	d.mu.Unlock()
	p, err := d.API.Product(ctx, item, store)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.products == nil {
		d.products = map[string]cached[*bunnings.Product]{}
	}
	d.products[key] = cached[*bunnings.Product]{p, time.Now()}
	d.mu.Unlock()
	return p, nil
}

func packagesOf(p *bunnings.Product) []shop.Package {
	if p == nil {
		return nil
	}
	dims := make([][3]string, len(p.Dimension.Packages))
	for i, x := range p.Dimension.Packages {
		dims[i] = [3]string{x.Length, x.Width, x.Height}
	}
	return shop.Packages(dims)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }

// textAndJSON returns a short summary for the model to read first, followed by
// the full structured result as JSON for clients that do not surface
// structuredContent.
func textAndJSON(summary string, out any) *mcp.CallToolResult {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: summary},
		&mcp.TextContent{Text: string(b)},
	}}
}
