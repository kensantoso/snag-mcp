package shop

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
)

// Document is a downloadable file on a product page.
type Document struct {
	Type string `json:"type"` // Bunnings' own label: Installation, Specification, Material safety, ...
	URL  string `json:"url"`
	// Format is "pdf" when Bunnings says so; empty when it does not.
	Format    string `json:"format,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"` // omitted when Bunnings reports 0
}

// Documents returns a product's downloads, keeping the real links: https
// URLs on Bunnings' own hosts, once each, in Bunnings' order. A link anywhere
// else is not passed on for an assistant to send someone to. Never nil, so
// "none" is an empty list.
func Documents(p *bunnings.Product) []Document {
	out := []Document{}
	var seen []string
	for _, d := range p.GuideDocument {
		u := strings.TrimSpace(d.URL)
		if !bunningsLink(u) || slices.Contains(seen, u) {
			continue
		}
		seen = append(seen, u)
		doc := Document{Type: strings.TrimSpace(d.DocType), URL: u, SizeBytes: max(d.Size, 0)}
		if doc.Type == "" {
			doc.Type = "Document"
		}
		if strings.Contains(strings.ToLower(d.Mime), "pdf") {
			doc.Format = "pdf"
		}
		out = append(out, doc)
	}
	return out
}

// bunningsLink reports an https URL on bunnings.com.au or one of its
// subdomains (media.bunnings.com.au serves the PDFs), with no credentials or
// port to dress another host up as one.
func bunningsLink(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "bunnings.com.au" || strings.HasSuffix(h, ".bunnings.com.au")
}

// Spec is one classification feature: Bunnings' name and its value, verbatim.
type Spec struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Specs flattens a product's classifications, values joined, names and
// values trimmed but otherwise as published.
func Specs(p *bunnings.Product) []Spec {
	var out []Spec
	for _, c := range p.Classifications {
		for _, f := range c.Features {
			var vals []string
			for _, v := range f.Values {
				if v := strings.TrimSpace(v.Value); v != "" {
					vals = append(vals, v)
				}
			}
			if len(vals) > 0 {
				out = append(out, Spec{strings.TrimSpace(f.Name), strings.Join(vals, ", ")})
			}
		}
	}
	return out
}

// ConnectionDetail is one statement Bunnings makes about how a fitting
// connects: thread type, gender, size, push-fit system, compatible pipe.
type ConnectionDetail struct {
	Source string `json:"source"` // spec, name, key_point or description
	Text   string `json:"text"`
}

var (
	connSpecRE = regexp.MustCompile(`(?i)size|thread|bsp|npt|connect|fitting|inlet|outlet|gender|male|compatib|push|barb|diameter|tube|pipe|hose|tap\b`)
	connTextRE = regexp.MustCompile(`(?i)\b(bspt?|npt|thread(ed|s)?|male|female|compression|push[- ]?fit|push[- ]?to[- ]?(connect|fit)|barb(ed)?|snap[- ]?on|lugged|solvent[- ]?weld|crimp|press[- ]?fit|nut (and|&) tail|hose tail|tap adaptor)\b`)
	// Some brands code ends into names: 15C (compression), 15MI,
	// 20FI, or a bare MI or FI ("Dual MI Hose Tap"). Matched case-sensitively
	// so "15 m" of hose or "Mi" in a word is not one.
	connCodeRE  = regexp.MustCompile(`\b(\d+(C|MI|FI|MF)|MI|FI)\b`)
	sentenceEnd = regexp.MustCompile(`(?:[.!?])\s+|\n+`)
	tempRE      = regexp.MustCompile(`(?i)temperature|degree|°`)
)

// hasConnCode reports an end code in s. A number and C is a temperature, not
// a compression end, when signed ("-50C to +70C") or said to be one.
func hasConnCode(s string) bool {
	for _, m := range connCodeRE.FindAllStringSubmatchIndex(s, -1) {
		if s[m[1]-1] == 'C' && (m[0] > 0 && strings.ContainsRune("-+", rune(s[m[0]-1])) || tempRE.MatchString(s)) {
			continue
		}
		return true
	}
	return false
}

// ConnectionDetails gathers, from everything Bunnings publishes about an item,
// the statements that say how it connects, verbatim and labelled with where
// they came from. It interprets nothing: a spec of "15" stays "15", because
// Bunnings states no unit or thread standard with it and neither may we.
func ConnectionDetails(name string, keyPoints []string, description string, specs []Spec) []ConnectionDetail {
	out := []ConnectionDetail{}
	add := func(src, text string) {
		text = strings.TrimSpace(text)
		if text == "" || slices.ContainsFunc(out, func(c ConnectionDetail) bool { return c.Text == text }) {
			return
		}
		out = append(out, ConnectionDetail{src, text})
	}
	text := func(s string) bool { return connTextRE.MatchString(s) || hasConnCode(s) }
	for _, s := range specs {
		if connSpecRE.MatchString(s.Name) {
			add("spec", s.Name+": "+s.Value)
		}
	}
	if text(name) {
		add("name", name)
	}
	for _, p := range keyPoints {
		if text(p) {
			add("key_point", p)
		}
	}
	for _, s := range sentenceEnd.Split(description, -1) {
		if text(s) {
			add("description", s)
		}
	}
	return out
}
