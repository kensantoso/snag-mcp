package geo

import (
	"strings"
	"testing"
)

func TestResolvePostcodes(t *testing.T) {
	for _, c := range []struct {
		in       string
		label    string
		lat, lon float64 // within ~15 km
	}{
		{"3194", "Mentone VIC 3194", -37.98, 145.07},
		{"6000", "Perth WA 6000", -31.95, 115.86},
		{"6530", "Geraldton WA 6530", -28.77, 114.62}, // one locality is an offshore island
		{"0870", "Alice Springs NT 0870", -23.70, 133.88},
		{"870", "Alice Springs NT 0870", -23.70, 133.88}, // leading zero dropped, as people write it
		{"7000", "Hobart TAS 7000", -42.88, 147.32},
	} {
		p, err := Resolve(c.in)
		if err != nil {
			t.Errorf("Resolve(%q): %v", c.in, err)
			continue
		}
		if p.Label() != c.label || DistanceKM(p.Lat, p.Lon, c.lat, c.lon) > 15 {
			t.Errorf("Resolve(%q) = %s %+v, want %s near %.2f,%.2f", c.in, p.Label(), p, c.label, c.lat, c.lon)
		}
	}
}

func TestResolveSuburbs(t *testing.T) {
	p, err := Resolve("mentone")
	if err != nil || p.Postcode != "3194" || p.State != "VIC" {
		t.Fatalf("Resolve(mentone) = %+v, %v", p, err)
	}
	// Richmond exists in several states; the state settles it.
	if _, err := Resolve("Richmond"); err == nil || !strings.Contains(err.Error(), "more than one state") {
		t.Errorf("Resolve(Richmond) err = %v, want ambiguity", err)
	}
	p, err = Resolve("Richmond, VIC")
	if err != nil || p.State != "VIC" || p.Postcode != "3121" {
		t.Errorf("Resolve(Richmond, VIC) = %+v, %v", p, err)
	}
	p, err = Resolve("Moorabbin Airport 3194")
	if err != nil || p.Suburb != "Moorabbin Airport" {
		t.Errorf("suburb plus postcode = %+v, %v", p, err)
	}
}

func TestResolveRejects(t *testing.T) {
	for _, in := range []string{"", "9999", "Atlantis"} {
		if p, err := Resolve(in); err == nil {
			t.Errorf("Resolve(%q) = %+v, want an error", in, p)
		}
	}
}

func TestDistance(t *testing.T) {
	// Melbourne to Perth is about 2,720 km.
	if d := DistanceKM(-37.81, 144.96, -31.95, 115.86); d < 2650 || d > 2800 {
		t.Errorf("Melbourne-Perth = %.0f km", d)
	}
}
