package shop

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// ParseLengthMM reads a package dimension such as "4.8 mtr", "450 mm" or
// "45cm" into millimetres. Unknown units are not guessed at.
func ParseLengthMM(s string) (float64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	var scale float64
	switch strings.TrimSpace(s[i:]) {
	case "mm":
		scale = 1
	case "cm":
		scale = 10
	case "m", "mtr", "mtrs", "metre", "metres", "meter", "meters":
		scale = 1000
	default:
		return 0, false
	}
	return math.Round(v*scale*10) / 10, true
}

// Package is one shipping package, in millimetres, largest dimension first.
type Package struct {
	LengthMM float64 `json:"length_mm"`
	WidthMM  float64 `json:"width_mm"`
	HeightMM float64 `json:"height_mm"`
}

// Packages normalises package dimensions, dropping any it cannot read in full
// and collapsing duplicates (an item can list the same package many times).
func Packages(dims [][3]string) []Package {
	var out []Package
	seen := map[Package]bool{}
	for _, d := range dims {
		var v [3]float64
		ok := true
		for i, s := range d {
			if v[i], ok = ParseLengthMM(s); !ok {
				break
			}
		}
		if !ok {
			continue
		}
		// Sides are reported in no consistent order (length is sometimes the
		// short side), and fitting a vehicle only cares about the longest.
		a, b, c := v[0], v[1], v[2]
		if a < b {
			a, b = b, a
		}
		if b < c {
			b, c = c, b
		}
		if a < b {
			a, b = b, a
		}
		p := Package{a, b, c}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// ParseWeightKG reads the product weight, which arrives as a string, a number,
// or not at all. Zero or negative is unknown, not weightless.
func ParseWeightKG(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.ToLower(s), "kg")), 64); err != nil {
			return nil
		}
	}
	if f <= 0 {
		return nil
	}
	return &f
}

// LoadItem is one list line as the load summary sees it.
type LoadItem struct {
	Item     string
	Name     string
	Qty      int
	WeightKG *float64
	Packages []Package
}

// Longest names the item with the longest package.
type Longest struct {
	ItemNumber string  `json:"item_number"`
	Name       string  `json:"name"`
	LengthM    float64 `json:"length_m"`
}

// Load is what the trip home has to carry, from known figures only.
type Load struct {
	Longest *Longest `json:"longest_item,omitempty"`
	// KnownWeightKG sums weight times quantity over items whose weight is
	// published. It is a floor, not an estimate, whenever WeightUnknown is set.
	KnownWeightKG float64  `json:"total_known_weight_kg"`
	WeightUnknown []string `json:"weight_unknown,omitempty"`
	SizeUnknown   []string `json:"size_unknown,omitempty"`
}

// Summarise totals a list's load. Missing weights and sizes are listed by name
// rather than estimated.
func Summarise(items []LoadItem) Load {
	var l Load
	for _, it := range items {
		if it.Qty <= 0 {
			continue
		}
		label := it.Name
		if label == "" {
			label = it.Item
		}
		if it.WeightKG != nil {
			l.KnownWeightKG += *it.WeightKG * float64(it.Qty)
		} else {
			l.WeightUnknown = append(l.WeightUnknown, label)
		}
		if len(it.Packages) == 0 {
			l.SizeUnknown = append(l.SizeUnknown, label)
			continue
		}
		for _, p := range it.Packages {
			if l.Longest == nil || p.LengthMM/1000 > l.Longest.LengthM {
				l.Longest = &Longest{ItemNumber: it.Item, Name: it.Name, LengthM: p.LengthMM / 1000}
			}
		}
	}
	l.KnownWeightKG = math.Round(l.KnownWeightKG*100) / 100
	return l
}
