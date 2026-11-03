package bunnings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeViewer serves a manifest with two stores, one of which has a plan but
// no walking graph, and counts requests per path. Like the real one, it turns
// away anything without a browser User-Agent.
type fakeViewer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	down   bool
	broken string // one path that fails
}

func newFakeViewer(t *testing.T) *fakeViewer {
	v := &fakeViewer{hits: map[string]int{}}
	files := map[string]string{
		"/api/stores-manifest.json":      `{"stores":{"0101":{"mapId":"0101-0003"},"0201":{"mapId":"0201-0002"}}}`,
		"/api/0101/0101-0003-map.json":   `{"mapId":"0101-0003"}`,
		"/api/0101/0101-0003-nodes.json": `{"resolution":1,"data":{}}`,
		"/api/0101/0101-0003-edges.json": `{"resolution":1,"data":[]}`,
		"/api/0201/0201-0002-map.json":   `{"mapId":"0201-0002"}`,
	}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.hits[r.URL.Path]++
		down := v.down || v.broken == r.URL.Path
		v.mu.Unlock()
		if !strings.Contains(r.UserAgent(), "Chrome/") || r.Referer() != v.URL+"/" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<html>Attention Required!</html>")
			return
		}
		if down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, b)
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *fakeViewer) count(path string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hits[path]
}

func mapClient(v *fakeViewer, dir string) *Client {
	c := New(2)
	c.MapBaseURL, c.MapCacheDir = v.URL, dir
	return c
}

func TestStoreMapFetchAndDiskCache(t *testing.T) {
	v := newFakeViewer(t)
	dir := t.TempDir()
	ctx := context.Background()

	f, err := mapClient(v, dir).StoreMap(ctx, "0101")
	if err != nil {
		t.Fatal(err)
	}
	if f.MapID != "0101-0003" || string(f.Map) != `{"mapId":"0101-0003"}` || f.Nodes == nil || f.Edges == nil {
		t.Fatalf("got %+v", f)
	}
	// A new process: everything from disk, nothing from the network.
	if _, err := mapClient(v, dir).StoreMap(ctx, "0101"); err != nil {
		t.Fatal(err)
	}
	if n := v.count("/api/0101/0101-0003-edges.json"); n != 1 {
		t.Errorf("edges fetched %d times, want 1", n)
	}

	// Older than the refresh age: fetched again.
	old := time.Now().Add(-mapMaxAge - time.Hour)
	for _, p := range []string{"stores-manifest.json", "0101/0101-0003-map.json"} {
		os.Chtimes(filepath.Join(dir, p), old, old)
	}
	if _, err := mapClient(v, dir).StoreMap(ctx, "0101"); err != nil {
		t.Fatal(err)
	}
	if n := v.count("/api/0101/0101-0003-map.json"); n != 2 {
		t.Errorf("stale plan fetched %d times in all, want 2", n)
	}

	// Stale and the viewer is down: the old copy beats no map.
	for _, p := range []string{"stores-manifest.json", "0101/0101-0003-map.json"} {
		os.Chtimes(filepath.Join(dir, p), old, old)
	}
	v.mu.Lock()
	v.down = true
	v.mu.Unlock()
	if f, err := mapClient(v, dir).StoreMap(ctx, "0101"); err != nil || f.MapID != "0101-0003" {
		t.Errorf("viewer down with a cached plan: %v, %v", f, err)
	}
}

func TestStoreMapMissing(t *testing.T) {
	v := newFakeViewer(t)
	ctx := context.Background()
	c := mapClient(v, t.TempDir())

	if _, err := c.StoreMap(ctx, "9999"); !errors.Is(err, ErrNoStoreMap) {
		t.Errorf("store not in the manifest: %v", err)
	}
	// A plan without a walking graph is still a plan.
	f, err := c.StoreMap(ctx, "0201")
	if err != nil || f.Map == nil || f.Nodes != nil || f.Edges != nil || f.GraphErr != nil {
		t.Errorf("plan without a graph: %+v, %v", f, err)
	}
	// A graph that is published but cannot be fetched: the plan, and why
	// there is no graph.
	v.mu.Lock()
	v.broken = "/api/0101/0101-0003-edges.json"
	v.mu.Unlock()
	f, err = mapClient(v, t.TempDir()).StoreMap(ctx, "0101")
	if err != nil || f.Map == nil || f.Nodes != nil || f.GraphErr == nil || !strings.Contains(f.GraphErr.Error(), "HTTP 502") {
		t.Errorf("graph unfetchable: %+v, %v", f, err)
	}
	// Nothing cached and the viewer refuses: an error, not a map.
	c = mapClient(v, t.TempDir())
	c.MapBaseURL = v.URL + "/nope"
	if _, err := c.StoreMap(ctx, "0101"); err == nil || errors.Is(err, ErrNoStoreMap) {
		t.Errorf("unreachable viewer: %v", err)
	}
}

func TestStoreMapPrunesSupersededPlans(t *testing.T) {
	v := newFakeViewer(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "0101"), 0o755)
	stale := filepath.Join(dir, "0101", "0101-0002-edges.json")
	os.WriteFile(stale, []byte("{}"), 0o644)
	if _, err := mapClient(v, dir).StoreMap(context.Background(), "0101"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the previous plan's files are still on disk")
	}
}

func TestStoreMapCorruptCacheIsRefetched(t *testing.T) {
	v := newFakeViewer(t)
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := mapClient(v, dir).StoreMap(ctx, "0101"); err != nil {
		t.Fatal(err)
	}
	// A fresh but truncated plan, as a crash mid-write would have left it
	// before writes went through a temporary file.
	path := filepath.Join(dir, "0101", "0101-0003-map.json")
	os.WriteFile(path, []byte(`{"mapId":"0101-00`), 0o644)
	f, err := mapClient(v, dir).StoreMap(ctx, "0101")
	if err != nil || string(f.Map) != `{"mapId":"0101-0003"}` {
		t.Fatalf("got %q, %v", f.Map, err)
	}
	if n := v.count("/api/0101/0101-0003-map.json"); n != 2 {
		t.Errorf("plan fetched %d times, want a refetch", n)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"mapId":"0101-0003"}` {
		t.Errorf("cache not repaired: %q", b)
	}
	// Viewer down and only a corrupt copy: an error, not the corrupt copy.
	os.WriteFile(path, []byte(`{"mapId"`), 0o644)
	old := time.Now().Add(-mapMaxAge - time.Hour)
	os.Chtimes(path, old, old)
	v.mu.Lock()
	v.down = true
	v.mu.Unlock()
	if _, err := mapClient(v, dir).StoreMap(ctx, "0101"); err == nil {
		t.Error("a corrupt cached plan was used")
	}
}

func TestWriteAtomicConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "f.json")
	big := []byte(`{"a":"` + strings.Repeat("x", 1<<20) + `"}`)
	small := []byte(`{"b":1}`)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := small
			if i%2 == 0 {
				b = big
			}
			writeAtomic(path, b)
		}()
	}
	wg.Wait()
	got, _ := os.ReadFile(path)
	if string(got) != string(big) && string(got) != string(small) {
		t.Errorf("file is a mix of writes: %d bytes", len(got))
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Errorf("left behind %d files", len(ents))
	}
}
