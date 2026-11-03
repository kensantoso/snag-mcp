package shop

import (
	"slices"
	"strings"
	"unicode"
)

// NormaliseBrand reduces a brand to lower-case letters and digits, so that
// "FlowLink", "flowlink" and "Flow-Link" compare equal.
func NormaliseBrand(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MatchBrands finds the brands a user meant among those sold nearby.
//
// In order: brands equal to the input once normalised, together with brand
// families that start with it as a whole word ("Gumleaf" also takes "Gumleaf
// Plumbing", but "Pipe" does not take "PipeWolf"); then, if none, a single
// brand that starts with the input ("pipew" is PipeWolf); then the closest
// spellings within a small edit distance ("pipewulf"). When nothing matches,
// suggestions holds the nearest spellings, for the caller to show.
func MatchBrands(input string, brands []string) (matched, suggestions []string) {
	n := NormaliseBrand(input)
	if n == "" {
		return nil, nil
	}
	words := strings.Fields(strings.ToLower(input))
	for _, b := range brands {
		if NormaliseBrand(b) == n || wordPrefix(strings.Fields(strings.ToLower(b)), words) {
			matched = append(matched, b)
		}
	}
	if len(matched) > 0 {
		return matched, nil
	}
	if len(n) >= 3 {
		var pre []string
		for _, b := range brands {
			if strings.HasPrefix(NormaliseBrand(b), n) {
				pre = append(pre, b)
			}
		}
		if len(pre) == 1 {
			return pre, nil
		}
		if len(pre) > 1 {
			slices.SortStableFunc(pre, func(a, b string) int { return len(a) - len(b) })
			return nil, pre[:min(8, len(pre))]
		}
	}
	tol := 1
	switch {
	case len(n) > 10:
		tol = 3
	case len(n) > 5:
		tol = 2
	}
	type cand struct {
		b string
		d int
	}
	var cs []cand
	for _, b := range brands {
		if d := levenshtein(n, NormaliseBrand(b)); d <= max(tol, len(n)/2) {
			cs = append(cs, cand{b, d})
		}
	}
	slices.SortStableFunc(cs, func(a, b cand) int { return a.d - b.d })
	for _, c := range cs {
		if c.d <= tol && c.d == cs[0].d {
			matched = append(matched, c.b)
		}
	}
	if len(matched) > 0 && len(matched) <= 3 {
		return matched, nil
	}
	for _, c := range cs[:min(8, len(cs))] {
		suggestions = append(suggestions, c.b)
	}
	return nil, suggestions
}

func wordPrefix(brand, input []string) bool {
	if len(input) == 0 || len(brand) <= len(input) {
		return false
	}
	for i, w := range input {
		if NormaliseBrand(brand[i]) != NormaliseBrand(w) {
			return false
		}
	}
	return true
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// BrandQuery is the Coveo clause restricting a search to the given brands.
// Values are the exact brand names from the facet; quotes cannot occur in a
// Coveo string literal and are dropped.
func BrandQuery(brands []string) string {
	q := make([]string, len(brands))
	for i, b := range brands {
		q[i] = `"` + strings.ReplaceAll(b, `"`, "") + `"`
	}
	return "@brandname==(" + strings.Join(q, ",") + ")"
}

// StripBrand removes the brand's words from a query once the brand is a
// filter. Left in, they are one more keyword every result must contain, and
// listings spell brands their own way ("FlowLink-PEX") so the filter alone is
// both stricter and more reliable. A query that is nothing but the brand
// ("FlowLink") is kept as it is: an empty query would rank the brand's whole
// range by nothing in particular.
func StripBrand(query string, brands ...string) string {
	toks := strings.Fields(query)
	for _, b := range brands {
		bw := strings.Fields(b)
		if len(bw) == 0 {
			continue
		}
		for i := 0; i+len(bw) <= len(toks); {
			same := true
			for j, w := range bw {
				if NormaliseBrand(toks[i+j]) != NormaliseBrand(w) {
					same = false
					break
				}
			}
			if same {
				toks = append(toks[:i], toks[i+len(bw):]...)
				continue
			}
			i++
		}
	}
	if len(toks) == 0 {
		return strings.TrimSpace(query)
	}
	return strings.Join(toks, " ")
}

// AnyWordQuery turns "pex 16mm female" into "pex OR 16mm OR female": a
// fallback for when every word together matches nothing, ranked by how many
// words each result has. Words that Coveo would read as operators are dropped.
func AnyWordQuery(query string) string {
	var ws []string
	for _, w := range strings.Fields(query) {
		switch strings.ToLower(w) {
		case "and", "or", "not", "near":
			continue
		}
		ws = append(ws, w)
	}
	if len(ws) < 2 {
		return ""
	}
	return strings.Join(ws, " OR ")
}
