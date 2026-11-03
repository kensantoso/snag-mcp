// Package geo turns an Australian postcode or suburb into a point, offline.
//
// The data is GeoNames' AU postal code extract, embedded so that resolving a
// location never sends the user's whereabouts to a third-party geocoder. It is
// licensed CC BY 4.0; see DATA_LICENSE beside this file.
package geo

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Attribution must accompany the data wherever it is redistributed.
const Attribution = "Postcode locations from GeoNames (geonames.org), CC BY 4.0."

//go:embed au_postcodes.tsv.gz
var data []byte

// Place is a resolved location.
type Place struct {
	Postcode string  `json:"postcode"`
	Suburb   string  `json:"suburb,omitempty"`
	State    string  `json:"state"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

// Label is how the place is named back to the user, e.g. "Mentone VIC 3194".
func (p Place) Label() string {
	if p.Suburb == "" {
		return p.State + " " + p.Postcode
	}
	return p.Suburb + " " + p.State + " " + p.Postcode
}

type entry struct {
	Place
	accuracy int
	council  string // local government area, e.g. "Greater Geraldton"
}

var (
	loadOnce sync.Once
	entries  []entry
	loadErr  error
)

func load() ([]entry, error) {
	loadOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			loadErr = err
			return
		}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			f := strings.Split(sc.Text(), "\t")
			if len(f) != 7 {
				continue
			}
			lat, e1 := strconv.ParseFloat(f[3], 64)
			lon, e2 := strconv.ParseFloat(f[4], 64)
			acc, _ := strconv.Atoi(f[5])
			if e1 != nil || e2 != nil {
				continue
			}
			entries = append(entries, entry{Place{Postcode: f[0], Suburb: f[1], State: f[2], Lat: lat, Lon: lon}, acc, f[6]})
		}
		loadErr = sc.Err()
	})
	return entries, loadErr
}

var states = map[string]bool{"VIC": true, "NSW": true, "QLD": true, "SA": true, "WA": true, "TAS": true, "NT": true, "ACT": true}

// Resolve turns "3194", "Mentone", "Mentone VIC" or "Mentone 3194" into a
// place. An ambiguous suburb is an error naming the candidates, because picking
// one silently would shop at the wrong end of the country.
func Resolve(input string) (Place, error) {
	all, err := load()
	if err != nil {
		return Place{}, err
	}
	var postcode, state string
	var words []string
	for _, w := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		switch {
		case isDigits(w) && len(w) >= 3 && len(w) <= 4:
			postcode = fmt.Sprintf("%04s", w)
		case states[strings.ToUpper(w)]:
			state = strings.ToUpper(w)
		default:
			words = append(words, w)
		}
	}
	suburb := strings.Join(words, " ")
	if postcode == "" && suburb == "" {
		return Place{}, fmt.Errorf("give an Australian postcode or suburb, e.g. 3194 or \"Mentone VIC\"")
	}

	if postcode != "" {
		var in []entry
		for _, e := range all {
			if e.Postcode == postcode {
				in = append(in, e)
			}
		}
		if len(in) == 0 {
			return Place{}, fmt.Errorf("%s is not an Australian postcode", postcode)
		}
		if suburb != "" {
			for _, e := range in {
				if strings.EqualFold(e.Suburb, suburb) {
					return e.Place, nil
				}
			}
		}
		return centroid(in), nil
	}

	var matches []entry
	for _, e := range all {
		if strings.EqualFold(e.Suburb, suburb) && (state == "" || e.State == state) {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return Place{}, fmt.Errorf("no suburb called %q%s; try the postcode instead", suburb, inState(state))
	}
	// The same suburb under several postcodes in one state is one place to us.
	byState := map[string]entry{}
	for _, m := range matches {
		if cur, ok := byState[m.State]; !ok || m.accuracy > cur.accuracy {
			byState[m.State] = m
		}
	}
	if len(byState) > 1 {
		var opts []string
		for _, m := range byState {
			opts = append(opts, m.Label())
		}
		sort.Strings(opts)
		return Place{}, fmt.Errorf("%q is in more than one state: %s. Add the state or postcode", suburb, strings.Join(opts, "; "))
	}
	for _, m := range byState {
		return m.Place, nil
	}
	panic("unreachable")
}

// centroid places a postcode at the median of its localities, so one outlier
// (a postcode reaching an offshore island) does not drag the point to sea.
// It is named for the locality its council is named for, which is the name
// people use (Geraldton, not Wonthella), else the locality nearest the point.
func centroid(in []entry) Place {
	use := in
	var precise []entry
	for _, e := range in {
		if e.accuracy >= 4 {
			precise = append(precise, e)
		}
	}
	if len(precise) > 0 {
		use = precise
	}
	lats := make([]float64, len(use))
	lons := make([]float64, len(use))
	for i, e := range use {
		lats[i], lons[i] = e.Lat, e.Lon
	}
	p := Place{Postcode: in[0].Postcode, State: in[0].State, Lat: median(lats), Lon: median(lons)}
	for _, e := range use {
		if strings.Contains(e.council, e.Suburb) {
			p.Suburb = e.Suburb
			return p
		}
	}
	best := math.Inf(1)
	for _, e := range use {
		if d := DistanceKM(p.Lat, p.Lon, e.Lat, e.Lon); d < best {
			best, p.Suburb = d, e.Suburb
		}
	}
	return p
}

func median(xs []float64) float64 {
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// DistanceKM is the great-circle distance between two points.
func DistanceKM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dlat, dlon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func inState(s string) string {
	if s == "" {
		return ""
	}
	return " in " + s
}
