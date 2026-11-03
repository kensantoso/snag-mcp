package shop

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
)

// testdata/products holds invented /v1/products responses in the shape the
// product API returns, with the fields the server reads.
func product(t *testing.T, item string) *bunnings.Product {
	t.Helper()
	b, err := os.ReadFile("testdata/products/" + item + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var p bunnings.Product
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestDocuments(t *testing.T) {
	// A pipe with one installation PDF.
	d := Documents(product(t, "9200001"))
	if len(d) != 1 || d[0].Type != "Installation" || d[0].Format != "pdf" || d[0].SizeBytes != 145977 ||
		!strings.HasPrefix(d[0].URL, "https://media.bunnings.com.au/") {
		t.Errorf("9200001 documents = %+v", d)
	}
	// A connector with three, in Bunnings' order.
	d = Documents(product(t, "9200002"))
	var types []string
	for _, x := range d {
		types = append(types, x.Type)
	}
	if strings.Join(types, ",") != "Brochure,Specification,User guide" {
		t.Errorf("9200002 types = %v", types)
	}
	// An e-scooter's safety sheet comes with no mime and no size: neither is
	// guessed.
	es := Documents(product(t, "9200003"))
	if len(es) != 2 {
		t.Errorf("9200003 documents = %+v", es)
	}
	for _, x := range es {
		if x.Type == "E-micromobility information" && (x.Format != "" || x.SizeBytes != 0) {
			t.Errorf("unknown format or size reported: %+v", x)
		}
	}
	// None is an empty list, not null, so "no documents" is explicit.
	if d := Documents(product(t, "9200004")); d == nil || len(d) != 0 {
		t.Errorf("9200004 documents = %#v, want empty", d)
	}
	b, _ := json.Marshal(Documents(product(t, "9200004")))
	if string(b) != "[]" {
		t.Errorf("marshals as %s", b)
	}
}

func TestDocumentsDropsNonLinksAndDuplicates(t *testing.T) {
	var p bunnings.Product
	json.Unmarshal([]byte(`{"guideDocument":[
		{"docType":"Manual","url":"https://media.bunnings.com.au/a","mime":"application/pdf","size":10},
		{"docType":"Manual","url":"https://media.bunnings.com.au/a","mime":"application/pdf","size":10},
		{"docType":"Manual","url":"","mime":"application/pdf"},
		{"docType":"","url":"javascript:void(0)"},
		{"docType":"","url":"https://media.bunnings.com.au/b","size":-1},
		{"docType":"Manual","url":"https://evil.example.com/manual.pdf"},
		{"docType":"Manual","url":"https://bunnings.com.au.evil.example/manual.pdf"},
		{"docType":"Manual","url":"https://notbunnings.com.au/manual.pdf"},
		{"docType":"Manual","url":"https://media.bunnings.com.au@evil.example/manual.pdf"},
		{"docType":"Manual","url":"http://media.bunnings.com.au/c"},
		{"docType":"Manual","url":"https://www.bunnings.com.au/d"},
		{"docType":"Manual","url":"https://MEDIA.Bunnings.com.au/e"}]}`), &p)
	d := Documents(&p)
	var urls []string
	for _, x := range d {
		urls = append(urls, x.URL)
	}
	want := []string{"https://media.bunnings.com.au/a", "https://media.bunnings.com.au/b", "https://www.bunnings.com.au/d", "https://MEDIA.Bunnings.com.au/e"}
	if !slices.Equal(urls, want) || d[1].Type != "Document" || d[1].SizeBytes != 0 {
		t.Errorf("documents = %+v", d)
	}
}

func connTexts(cs []ConnectionDetail) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		m[c.Text] = c.Source
	}
	return m
}

func TestConnectionDetails(t *testing.T) {
	cd := func(item string) map[string]string {
		p := product(t, item)
		return connTexts(ConnectionDetails(p.Name, p.Feature.Pointers, p.Feature.Description, Specs(p)))
	}
	// A tap adaptor: the thread is stated only in a key selling point, the
	// connection type only in a spec.
	got := cd("9200006")
	if got[`Suits 20mm (3/4") and 25mm (1") BSP taps`] != "key_point" || got["Fitting Connection Type: Snap on"] != "spec" {
		t.Errorf("9200006 = %v", got)
	}
	// A hose whose inlet and outlet are bare numbers, which stay bare.
	got = cd("9200007")
	if got["Inlet Size: 15"] != "spec" || got["Outlet Size: 15"] != "spec" {
		t.Errorf("9200007 = %v", got)
	}
	// No connection spec at all; the name carries the codes.
	got = cd("9200005")
	if got["Copperline 15C x 15MI Brass Compression Union LF"] != "name" {
		t.Errorf("9200005 = %v", got)
	}
	// Or a bare code: a male-iron (MI) tap.
	if got := cd("9200004"); got["Copperline Dual MI Hose Tap 20mm Chrome Lead Free"] != "name" {
		t.Errorf("9200004 = %v", got)
	}
	// Thread and gender in the name.
	if got := cd("9200002"); got["PipeWolf PEX Connector, 16mm × ½″ BSP Female Threaded"] != "name" {
		t.Errorf("9200002 = %v", got)
	}
	// A temperature is not a compression end.
	got = connTexts(ConnectionDetails("FlowLink-PEX 16 x 20mm Female Connector",
		[]string{"Recommended water temperature -50C to +70C degrees", "Suits -20C to +60C"}, "", nil))
	if len(got) != 1 || got["FlowLink-PEX 16 x 20mm Female Connector"] != "name" {
		t.Errorf("temperatures = %v", got)
	}
	// Nothing about connections is nothing, as an empty list.
	if got := ConnectionDetails("Example 18V Drill", []string{"Brushless motor", "15m of cable"}, "A drill. It drills.", []Spec{{"Colour", "Green"}}); len(got) != 0 {
		t.Errorf("drill = %v", got)
	}
}

func TestMatchBrands(t *testing.T) {
	// Invented brands, with the shapes real ones come in.
	brands := []string{"FlowLink", "Flowform", "FlowBurn", "PipeWolf", "Pipe", "Wombat Works", "Gumleaf",
		"Gumleaf Plumbing", "Copperline", "Dripstop", "Lockwell Smart Door Locks", "Lockwell Smart Safes"}
	for _, c := range []struct {
		in, want, suggest string
	}{
		{"flowlink", "FlowLink", ""},
		{"FLOWLINK", "FlowLink", ""},
		{"Flow-Link", "FlowLink", ""},
		{"pipewulf", "PipeWolf", ""},                // misspelt
		{"pipe wolf", "PipeWolf", ""},               // spaced
		{"pipe", "Pipe", ""},                        // a word prefix of PipeWolf is not PipeWolf
		{"wombatworks", "Wombat Works", ""},         // run together
		{"gumleaf", "Gumleaf,Gumleaf Plumbing", ""}, // a family
		{"copper line", "Copperline", ""},
		{"lockwell", "Lockwell Smart Door Locks,Lockwell Smart Safes", ""}, // a family
		{"flow", "", "FlowLink"},                                           // prefix of several: ask
		{"zzzz", "", ""},
		{"", "", ""},
	} {
		m, s := MatchBrands(c.in, brands)
		if strings.Join(m, ",") != c.want {
			t.Errorf("MatchBrands(%q) = %v, want %q", c.in, m, c.want)
		}
		if c.suggest != "" && !strings.Contains(strings.Join(s, ","), c.suggest) {
			t.Errorf("MatchBrands(%q) suggestions = %v, want %q among them", c.in, s, c.suggest)
		}
	}
}

func TestBrandQueryAndStrip(t *testing.T) {
	if q := BrandQuery([]string{"FlowLink", `Bob "the" Builder`}); q != `@brandname==("FlowLink","Bob the Builder")` {
		t.Errorf("BrandQuery = %s", q)
	}
	for _, c := range []struct{ q, want string }{
		{"FlowLink PEX 16mm female", "PEX 16mm female"},
		{"flowlink-pex 16mm", "flowlink-pex 16mm"}, // a different token, left alone
		{"wombat works drill bits", "drill bits"},
		{"FlowLink", "FlowLink"}, // nothing but the brand: kept
		{" wombat  works ", "wombat  works"},
	} {
		if got := StripBrand(c.q, "flowlink", "FlowLink", "Wombat Works"); got != c.want {
			t.Errorf("StripBrand(%q) = %q, want %q", c.q, got, c.want)
		}
	}
	if got := AnyWordQuery("pex 16mm or female"); got != "pex OR 16mm OR female" {
		t.Errorf("AnyWordQuery = %q", got)
	}
	if got := AnyWordQuery("pex"); got != "" {
		t.Errorf("one word needs no fallback, got %q", got)
	}
}
