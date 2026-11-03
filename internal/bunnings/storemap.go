package bunnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The store floor plans come from Bunnings' map viewer, not the gateway. It
// sits behind a bot filter that turns away requests without a browser
// User-Agent, and the viewer sends a Referer, so we do too.
const (
	mapViewer = "https://map-viewer.apps.bunnings.com.au"
	// mapMaxAge is how long a downloaded plan is used before checking for a
	// newer one. The viewer itself caches them for a year; plans change when
	// a store is refitted, and a new plan has a new map id.
	mapMaxAge = 3 * 24 * time.Hour
)

// ErrNoStoreMap means Bunnings publishes no floor plan for the store.
var ErrNoStoreMap = errors.New("bunnings publishes no floor plan for this store")

// StoreMapFiles are a store's floor plan and walking graph, as published.
// Nodes and Edges are nil when the store has a plan but no walking graph:
// GraphErr says why when Bunnings has one that could not be fetched, and is
// nil when it publishes none.
type StoreMapFiles struct {
	MapID        string
	Map          []byte
	Nodes, Edges []byte
	GraphErr     error
}

// DefaultMapCacheDir is where floor plans are kept between runs.
func DefaultMapCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "snag-mcp", "maps")
}

// StoreMap fetches a store's floor plan and walking graph, from the disk
// cache when it is fresh. The walking graph is several megabytes for a large
// store, so it is fetched only for a store someone asks about.
func (c *Client) StoreMap(ctx context.Context, store string) (*StoreMapFiles, error) {
	b, err := c.mapFile(ctx, "stores-manifest.json")
	if err != nil {
		return nil, fmt.Errorf("store maps: %w", err)
	}
	var man struct {
		Stores map[string]struct {
			MapID string `json:"mapId"`
		} `json:"stores"`
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, fmt.Errorf("store maps: manifest: %w", err)
	}
	id := man.Stores[store].MapID
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, ErrNoStoreMap
	}
	out := &StoreMapFiles{MapID: id}
	base := store + "/" + id
	if out.Map, err = c.mapFile(ctx, base+"-map.json"); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, ErrNoStoreMap
		}
		return nil, fmt.Errorf("store map: %w", err)
	}
	c.pruneMaps(store, id)
	// Without a graph the walk is ordered by straight lines instead.
	if out.Nodes, err = c.mapFile(ctx, base+"-nodes.json"); err == nil {
		out.Edges, err = c.mapFile(ctx, base+"-edges.json")
	}
	if err != nil {
		out.Nodes, out.Edges = nil, nil
		if !errors.Is(err, errNotFound) {
			out.GraphErr = err
		}
	}
	return out, nil
}

var errNotFound = errors.New("not found")

// mapFile returns one file under /api, from disk when younger than mapMaxAge.
// A failed refresh falls back to the copy on disk, however old: a plan from
// last month beats none. A copy on disk that is not JSON (truncated, or
// written over by something else) is never used.
func (c *Client) mapFile(ctx context.Context, rel string) ([]byte, error) {
	var path string
	cached := func() []byte {
		if b, err := os.ReadFile(path); err == nil && json.Valid(b) {
			return b
		}
		return nil
	}
	if c.MapCacheDir != "" {
		path = filepath.Join(c.MapCacheDir, filepath.FromSlash(rel))
		if st, err := os.Stat(path); err == nil && time.Since(st.ModTime()) < mapMaxAge {
			if b := cached(); b != nil {
				return b, nil
			}
		}
	}
	b, err := c.fetchMapFile(ctx, rel)
	if err != nil {
		if path != "" && !errors.Is(err, errNotFound) {
			if old := cached(); old != nil {
				return old, nil
			}
		}
		return nil, err
	}
	if path != "" {
		writeAtomic(path, b)
	}
	return b, nil
}

// writeAtomic writes b to a temporary file beside path and renames it into
// place, so neither a reader nor another process writing the same file at the
// same time ever sees half of it. A cache that cannot be written is no cache.
func writeAtomic(path string, b []byte) {
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(f.Name(), 0o644) != nil || os.Rename(f.Name(), path) != nil {
		os.Remove(f.Name())
	}
}

func (c *Client) fetchMapFile(ctx context.Context, rel string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.MapBaseURL+"/api/"+rel, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", c.MapBaseURL+"/")
	req.Header.Set("Accept", "application/json")
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()
	// The walking graph for a big store is over 5 MB; give it longer than a
	// gateway call.
	hc := &http.Client{Timeout: 90 * time.Second, Transport: c.HTTP.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d", rel, resp.StatusCode)
	case !json.Valid(b):
		// The bot filter answers some refusals with an HTML page.
		return nil, fmt.Errorf("%s: not JSON", rel)
	}
	return b, nil
}

// pruneMaps removes a store's superseded plans once a new map id is in use.
func (c *Client) pruneMaps(store, id string) {
	if c.MapCacheDir == "" {
		return
	}
	dir := filepath.Join(c.MapCacheDir, store)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), id+"-") {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
