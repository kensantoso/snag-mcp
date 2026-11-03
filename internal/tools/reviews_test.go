package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// When every review is syndicated, Bazaarvoice ignores a sort by helpful
// votes. The tool asks again for newest first, and says that is what they
// are, rather than claiming an order it did not ask for. A review with no
// date has no date, not 0001-01-01.
func TestReviewsSyndicatedAreRequestedNewestFirst(t *testing.T) {
	var sorts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config.js":
			fmt.Fprint(w, `{"mprAPIKey":"KEY0123456789abcdefghij"}`)
		case "/data/reviews.json":
			sort := r.URL.Query().Get("Sort")
			sorts = append(sorts, sort)
			date := `"2026-10-08T01:00:00.000+00:00"`
			if strings.HasPrefix(sort, "SubmissionTime") {
				date = `null`
			}
			fmt.Fprintf(w, `{"Results":[{"Rating":5,"Title":"Good","ReviewText":"Works","SubmissionTime":%s,
				"TotalPositiveFeedbackCount":0,"IsSyndicated":true,"SyndicationSource":{"Name":"Example Maker"}}],
				"Includes":{"Products":{"9300002":{"ReviewStatistics":{"TotalReviewCount":3339,"AverageOverallRating":4.6}}}}}`, date)
		case "/data/questions.json":
			fmt.Fprint(w, `{"TotalResults":0,"Results":[]}`)
		}
	}))
	defer srv.Close()
	api := bunnings.New(2)
	api.BVConfigURL, api.BVBaseURL = srv.URL+"/config.js", srv.URL+"/data"
	cs := sessionFor(t, api)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_reviews", Arguments: map[string]any{"item_number": "9300002"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	var out getReviewsOut
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	if len(sorts) != 2 || !strings.HasPrefix(sorts[0], "TotalPositiveFeedbackCount:desc") || sorts[1] != "SubmissionTime:desc" {
		t.Errorf("sorts requested = %q", sorts)
	}
	if out.Sort != "recent" || !strings.Contains(out.Note, "syndicated from Example Maker") || len(out.Reviews) != 1 || out.Reviews[0].Date != "" {
		t.Errorf("out = %+v", out)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "newest first") || strings.Contains(text, "most helpful") || strings.Contains(text, "0001") || strings.Contains(text, "(, ") {
		t.Errorf("text = %s", text)
	}
	if strings.Contains(string(res.Content[1].(*mcp.TextContent).Text), `"date"`) {
		t.Error("an absent date is in the JSON")
	}
}
