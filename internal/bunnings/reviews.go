package bunnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reviews and questions are not Bunnings' own data: the product page loads
// them from Bazaarvoice with a public read key that its widget config carries.
// The only thing sent there is an item number.
const (
	bvConfigURL = "https://apps.bazaarvoice.com/deployments/bunnings-nz/main_site/production/en_AU/ratings-config.js"
	bvAPI       = "https://api.bazaarvoice.com/data"
	// bvFallbackKey is mprAPIKey as published on 2026-11-03. The key is read
	// from the live config first, so a rotation is picked up without a
	// release; this is used only when the config cannot be read.
	bvFallbackKey = "caYn2gPalUTJwFy5xsFBzCG3r8ajeT9oK2kg96AyAHnTQ"
)

var bvKeyRE = regexp.MustCompile(`"mprAPIKey"\s*:\s*"([A-Za-z0-9]{20,})"`)

// Review is one customer review.
type Review struct {
	Rating      int
	Title       string
	Text        string
	Submitted   time.Time
	Helpful     int
	NotHelpful  int
	Author      string
	Locale      string // en_AU or en_NZ: the review pool is shared with Bunnings NZ
	Recommended *bool
	// SyndicatedFrom names another retailer or the manufacturer when the
	// review was written elsewhere and shown at Bunnings.
	SyndicatedFrom string
}

// ReviewStats summarises every review of a product, text or not.
type ReviewStats struct {
	Count        int
	Average      float64
	Distribution map[int]int // stars -> count
	Recommended  int         // reviewers who answered yes
	RecommendOf  int         // reviewers who answered at all
}

// Answer is an answer to a customer question.
type Answer struct {
	Text      string
	Submitted time.Time
	Author    string
	Brand     bool // answered by the brand or manufacturer
	Helpful   int
}

// Question is a customer question with its answers.
type Question struct {
	Summary   string
	Details   string
	Submitted time.Time
	Author    string
	Answers   []Answer
}

// ReviewSort orders reviews.
type ReviewSort string

const (
	SortHelpful ReviewSort = "helpful"
	SortRecent  ReviewSort = "recent"
	SortLowest  ReviewSort = "lowest"
	SortHighest ReviewSort = "highest"
)

func (s ReviewSort) param() (string, error) {
	switch s {
	case "", SortHelpful:
		return "TotalPositiveFeedbackCount:desc,SubmissionTime:desc", nil
	case SortRecent:
		return "SubmissionTime:desc", nil
	case SortLowest:
		return "Rating:asc,TotalPositiveFeedbackCount:desc", nil
	case SortHighest:
		return "Rating:desc,TotalPositiveFeedbackCount:desc", nil
	}
	return "", fmt.Errorf("sort must be helpful, recent, lowest or highest")
}

// bvKey returns the Bazaarvoice read key: the one in the live widget config
// when it can be read, else the embedded one. A config that was read is kept
// for the life of the process; a failed read is retried on a later call.
func (c *Client) bvKey(ctx context.Context) string {
	c.mu.Lock()
	k := c.bvKeyCached
	c.mu.Unlock()
	if k != "" {
		return k
	}
	raw, err := c.fetch(ctx, c.BVConfigURL)
	if err == nil {
		if m := bvKeyRE.FindSubmatch(raw); m != nil {
			c.mu.Lock()
			c.bvKeyCached = string(m[1])
			c.mu.Unlock()
			return string(m[1])
		}
	}
	return bvFallbackKey
}

func (c *Client) fetch(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json, */*")
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, err := c.HTTP.Do(req)
	<-c.sem
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return raw, &APIError{Status: resp.StatusCode, Description: truncate(string(raw), 200)}
	}
	return raw, nil
}

type bvError struct {
	Code, Message string
}

// bv calls a Bazaarvoice endpoint. If the live key is rejected, the embedded
// one is tried once, and the cached key forgotten so the config is re-read
// next time.
func (c *Client) bv(ctx context.Context, path string, q url.Values, out any) error {
	q.Set("apiversion", "5.4")
	key := c.bvKey(ctx)
	for attempt := 0; ; attempt++ {
		q.Set("passkey", key)
		raw, err := c.fetch(ctx, c.BVBaseURL+path+"?"+q.Encode())
		var env struct {
			HasErrors bool      `json:"HasErrors"`
			Errors    []bvError `json:"Errors"`
		}
		if jerr := json.Unmarshal(raw, &env); jerr != nil {
			if err != nil {
				return fmt.Errorf("reviews: %w", err)
			}
			return fmt.Errorf("reviews: undecodable response: %w", jerr)
		}
		if env.HasErrors || len(env.Errors) > 0 {
			e := bvError{Code: "unknown"}
			if len(env.Errors) > 0 {
				e = env.Errors[0]
			}
			if strings.Contains(e.Code, "KEY") && attempt == 0 && key != bvFallbackKey {
				c.mu.Lock()
				c.bvKeyCached = ""
				c.mu.Unlock()
				key = bvFallbackKey
				continue
			}
			return fmt.Errorf("reviews: %s %s", e.Code, e.Message)
		}
		if err != nil {
			return fmt.Errorf("reviews: %w", err)
		}
		return json.Unmarshal(raw, out)
	}
}

type bvTime struct{ time.Time }

func (t *bvTime) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil || s == "" {
		return nil
	}
	if v, err := time.Parse("2006-01-02T15:04:05.000-07:00", s); err == nil {
		t.Time = v
	} else if v, err := time.Parse(time.RFC3339, s); err == nil {
		t.Time = v
	}
	return nil
}

// Reviews returns up to limit written reviews of an item, and statistics over
// all its reviews. Ratings-only reviews count in the statistics but are not
// listed: they have nothing to read.
func (c *Client) Reviews(ctx context.Context, item string, sort ReviewSort, limit int) ([]Review, ReviewStats, error) {
	if !ValidItemNumber(item) {
		return nil, ReviewStats{}, fmt.Errorf("invalid item number %q", item)
	}
	sp, err := sort.param()
	if err != nil {
		return nil, ReviewStats{}, err
	}
	q := url.Values{}
	q.Add("Filter", "ProductId:"+item)
	q.Add("Filter", "IsRatingsOnly:false")
	q.Set("Sort", sp)
	q.Set("Limit", strconv.Itoa(limit))
	q.Set("Include", "Products")
	q.Set("Stats", "Reviews")
	var data struct {
		Results []struct {
			Rating         int    `json:"Rating"`
			Title          string `json:"Title"`
			ReviewText     string `json:"ReviewText"`
			SubmissionTime bvTime `json:"SubmissionTime"`
			Positive       int    `json:"TotalPositiveFeedbackCount"`
			Negative       int    `json:"TotalNegativeFeedbackCount"`
			UserNickname   string `json:"UserNickname"`
			ContentLocale  string `json:"ContentLocale"`
			IsRecommended  *bool  `json:"IsRecommended"`
			IsSyndicated   bool   `json:"IsSyndicated"`
			Syndication    *struct {
				Name string `json:"Name"`
			} `json:"SyndicationSource"`
		} `json:"Results"`
		Includes struct {
			Products map[string]struct {
				ReviewStatistics struct {
					Total        int     `json:"TotalReviewCount"`
					Average      float64 `json:"AverageOverallRating"`
					Recommended  int     `json:"RecommendedCount"`
					NotRecommend int     `json:"NotRecommendedCount"`
					Distribution []struct {
						Value int `json:"RatingValue"`
						Count int `json:"Count"`
					} `json:"RatingDistribution"`
				} `json:"ReviewStatistics"`
			} `json:"Products"`
		} `json:"Includes"`
	}
	if err := c.bv(ctx, "/reviews.json", q, &data); err != nil {
		return nil, ReviewStats{}, err
	}
	var stats ReviewStats
	if p, ok := data.Includes.Products[item]; ok {
		rs := p.ReviewStatistics
		stats = ReviewStats{Count: rs.Total, Average: rs.Average, Distribution: map[int]int{},
			Recommended: rs.Recommended, RecommendOf: rs.Recommended + rs.NotRecommend}
		for _, d := range rs.Distribution {
			stats.Distribution[d.Value] = d.Count
		}
	}
	out := make([]Review, 0, len(data.Results))
	for _, r := range data.Results {
		rv := Review{
			Rating: r.Rating, Title: strings.TrimSpace(r.Title), Text: strings.TrimSpace(r.ReviewText),
			Submitted: r.SubmissionTime.Time, Helpful: r.Positive, NotHelpful: r.Negative,
			Author: r.UserNickname, Locale: r.ContentLocale, Recommended: r.IsRecommended,
		}
		if r.IsSyndicated {
			rv.SyndicatedFrom = "another site"
			if r.Syndication != nil && r.Syndication.Name != "" {
				rv.SyndicatedFrom = r.Syndication.Name
			}
		}
		out = append(out, rv)
	}
	return out, stats, nil
}

// Questions returns up to limit customer questions about an item, newest
// first, each with its answers, and how many questions there are in all.
func (c *Client) Questions(ctx context.Context, item string, limit int) ([]Question, int, error) {
	if !ValidItemNumber(item) {
		return nil, 0, errors.New("invalid item number")
	}
	q := url.Values{}
	q.Set("Filter", "ProductId:"+item)
	q.Set("Sort", "SubmissionTime:desc")
	q.Set("Limit", strconv.Itoa(limit))
	q.Set("Include", "Answers")
	var data struct {
		TotalResults int `json:"TotalResults"`
		Results      []struct {
			Summary   string   `json:"QuestionSummary"`
			Details   *string  `json:"QuestionDetails"`
			Submitted bvTime   `json:"SubmissionTime"`
			Author    string   `json:"UserNickname"`
			AnswerIDs []string `json:"AnswerIds"`
		} `json:"Results"`
		Includes struct {
			Answers map[string]struct {
				Text      string `json:"AnswerText"`
				Submitted bvTime `json:"SubmissionTime"`
				Author    string `json:"UserNickname"`
				Brand     bool   `json:"IsBrandAnswer"`
				Positive  int    `json:"TotalPositiveFeedbackCount"`
			} `json:"Answers"`
		} `json:"Includes"`
	}
	if err := c.bv(ctx, "/questions.json", q, &data); err != nil {
		return nil, 0, err
	}
	out := make([]Question, 0, len(data.Results))
	for _, r := range data.Results {
		qq := Question{Summary: strings.TrimSpace(r.Summary), Submitted: r.Submitted.Time, Author: r.Author}
		if r.Details != nil {
			qq.Details = strings.TrimSpace(*r.Details)
		}
		for _, id := range r.AnswerIDs {
			if a, ok := data.Includes.Answers[id]; ok {
				qq.Answers = append(qq.Answers, Answer{Text: strings.TrimSpace(a.Text), Submitted: a.Submitted.Time,
					Author: a.Author, Brand: a.Brand, Helpful: a.Positive})
			}
		}
		out = append(out, qq)
	}
	return out, data.TotalResults, nil
}
