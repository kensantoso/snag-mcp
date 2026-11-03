package bunnings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBV serves the widget config with configKey (or a 404 when it is empty)
// and invented review and question responses in Bazaarvoice's shape,
// accepting only acceptKey.
func fakeBV(t *testing.T, configKey, acceptKey string, seen *[]string) *httptest.Server {
	reviews, _ := os.ReadFile("testdata/reviews_9300001.json")
	questions, _ := os.ReadFile("testdata/questions_9300002.json")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.js":
			if configKey == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `window.BV=(function(){return {"siteId":"main_site","mprAPIKey":"%s","locale":"en_AU"}})();`, configKey)
			return
		}
		q := r.URL.Query()
		*seen = append(*seen, r.URL.Path+" "+q.Get("passkey"))
		if q.Get("passkey") != acceptKey {
			fmt.Fprint(w, `{"HasErrors":true,"Errors":[{"Code":"ERROR_PARAM_INVALID_API_KEY","Message":"Invalid passkey"}]}`)
			return
		}
		switch r.URL.Path {
		case "/data/reviews.json":
			if got := q["Filter"]; strings.Join(got, ",") != "ProductId:9300001,IsRatingsOnly:false" {
				t.Errorf("reviews filter = %v", got)
			}
			if q.Get("Sort") != "TotalPositiveFeedbackCount:desc,SubmissionTime:desc" || q.Get("apiversion") != "5.4" {
				t.Errorf("reviews query = %v", q)
			}
			w.Write(reviews)
		case "/data/questions.json":
			if q.Get("Filter") != "ProductId:9300002" || q.Get("Include") != "Answers" {
				t.Errorf("questions query = %v", q)
			}
			w.Write(questions)
		default:
			http.NotFound(w, r)
		}
	}))
}

func bvClient(s *httptest.Server) *Client {
	c := New(2)
	c.BVConfigURL, c.BVBaseURL = s.URL+"/config.js", s.URL+"/data"
	return c
}

func TestReviews(t *testing.T) {
	var seen []string
	s := fakeBV(t, "CONFIGKEY0123456789abcdef", "CONFIGKEY0123456789abcdef", &seen)
	defer s.Close()
	rs, st, err := bvClient(s).Reviews(context.Background(), "9300001", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	// 19 reviews averaging 2.63, 4 of 11 recommending.
	if st.Count != 19 || math.Abs(st.Average-2.63) > 0.01 || st.Recommended != 4 || st.RecommendOf != 11 {
		t.Errorf("stats = %+v", st)
	}
	if st.Distribution[1] != 9 || st.Distribution[5] != 5 || st.Distribution[2] != 1 {
		t.Errorf("distribution = %v", st.Distribution)
	}
	if len(rs) != 3 {
		t.Fatalf("%d reviews", len(rs))
	}
	r := rs[0]
	if r.Rating != 1 || r.Title != "Handle snapped after a month" || r.Helpful != 31 || r.Text == "" || r.Locale != "en_AU" || r.SyndicatedFrom != "" {
		t.Errorf("first review = %+v", r)
	}
	if !r.Submitted.Equal(time.Date(2021, 1, 25, 10, 1, 59, 0, time.UTC)) {
		t.Errorf("submitted %v", r.Submitted)
	}
	if r.Recommended == nil || *r.Recommended {
		t.Errorf("recommended = %v, want false", r.Recommended)
	}
	if rs[2].Recommended != nil {
		t.Error("a reviewer who did not say should be nil, not false")
	}
	if rs[1].SyndicatedFrom != "Example Maker" {
		t.Errorf("second review syndicated from %q", rs[1].SyndicatedFrom)
	}
}

func TestQuestions(t *testing.T) {
	var seen []string
	s := fakeBV(t, "CONFIGKEY0123456789abcdef", "CONFIGKEY0123456789abcdef", &seen)
	defer s.Close()
	qs, n, err := bvClient(s).Questions(context.Background(), "9300002", 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(qs) != 1 || !strings.HasPrefix(qs[0].Summary, "Will this cartridge fit") || qs[0].Details != "" {
		t.Fatalf("questions = %d %+v", n, qs)
	}
	if a := qs[0].Answers; len(a) != 1 || a[0].Author != "Example Brand Support" || !a[0].Brand || !strings.Contains(a[0].Text, "1300 000 000") {
		t.Errorf("answers = %+v", a)
	}
}

func TestReviewKey(t *testing.T) {
	ctx := context.Background()

	// The key comes from Bazaarvoice's config, once per process.
	var seen []string
	s := fakeBV(t, "ROTATEDKEY0123456789abcd", "ROTATEDKEY0123456789abcd", &seen)
	c := bvClient(s)
	c.Reviews(ctx, "9300001", "", 5)
	c.Questions(ctx, "9300002", 5)
	s.Close()
	if strings.Join(seen, "|") != "/data/reviews.json ROTATEDKEY0123456789abcd|/data/questions.json ROTATEDKEY0123456789abcd" {
		t.Errorf("requests = %v", seen)
	}

	// No config: the embedded key.
	seen = nil
	s = fakeBV(t, "", bvFallbackKey, &seen)
	if _, _, err := bvClient(s).Reviews(ctx, "9300001", "", 5); err != nil || len(seen) != 1 {
		t.Errorf("fallback: %v, requests %v", err, seen)
	}
	s.Close()

	// A config key Bazaarvoice rejects: one retry with the embedded key.
	seen = nil
	s = fakeBV(t, "WRONGKEY0123456789abcdefg", bvFallbackKey, &seen)
	if _, _, err := bvClient(s).Reviews(ctx, "9300001", "", 5); err != nil || len(seen) != 2 {
		t.Errorf("rejected key: %v, requests %v", err, seen)
	}
	s.Close()

	// Both rejected: an error, after exactly those two tries.
	seen = nil
	s = fakeBV(t, "WRONGKEY0123456789abcdefg", "NEITHER", &seen)
	if _, _, err := bvClient(s).Reviews(ctx, "9300001", "", 5); err == nil || !strings.Contains(err.Error(), "INVALID_API_KEY") || len(seen) != 2 {
		t.Errorf("both rejected: %v, requests %v", err, seen)
	}
	s.Close()
}

func TestSearchFacet(t *testing.T) {
	var issued atomic.Int32
	auth := fakeAuth(t, &issued)
	defer auth.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		g, _ := body["groupBy"].([]any)
		if len(g) != 1 || g[0].(map[string]any)["field"] != "@brandname" || g[0].(map[string]any)["maximumNumberOfValues"] != float64(5000) {
			t.Errorf("groupBy = %v", body["groupBy"])
		}
		fmt.Fprint(w, `{"statusDetails":{"state":"SUCCESS"},"data":{"totalCount":84,"results":[],
			"groupByResults":[{"field":"brandname","values":[{"value":"FlowLink","numberOfResults":60},{"value":"PipeWolf","numberOfResults":24}]}]}}`)
	}))
	defer gw.Close()
	c := New(2)
	c.AuthURL, c.BaseURL = auth.URL+"/connect/authorize", gw.URL
	res, err := c.Search(context.Background(), SearchRequest{AQ: "x", Store: "0101", Facet: "@brandname", FacetMax: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 84 || len(res.Facet) != 2 || res.Facet[0] != (FacetValue{"FlowLink", 60}) {
		t.Errorf("result = %+v", res)
	}
}
