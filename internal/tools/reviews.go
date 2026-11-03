package tools

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kensantoso/snag-mcp/internal/bunnings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultReviews = 5
	maxReviews     = 20
	maxQuestions   = 5
	// A review long enough to need more than this is rare; cutting there
	// keeps twenty of them from swamping the conversation.
	maxReviewChars = 1200
)

type getReviewsIn struct {
	ItemNumber string `json:"item_number" jsonschema:"Bunnings item number from search_products, e.g. '3129102'. Keep leading zeros."`
	Sort       string `json:"sort,omitempty" jsonschema:"helpful (default: most helpful first), recent, lowest (to find the problems) or highest."`
	Limit      int    `json:"limit,omitempty" jsonschema:"How many written reviews. Default 5, at most 20."`
}

type reviewOut struct {
	Rating     int    `json:"rating"`
	Title      string `json:"title,omitempty"`
	Text       string `json:"text"`
	Date       string `json:"date,omitempty"`
	Helpful    int    `json:"helpful_votes"`
	NotHelpful int    `json:"not_helpful_votes,omitempty"`
	Author     string `json:"author,omitempty"`
	Country    string `json:"country,omitempty"` // AU or NZ; Bunnings shares reviews across both
	Recommends *bool  `json:"recommends,omitempty"`
	Syndicated string `json:"syndicated_from,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type answerOut struct {
	Text    string `json:"text"`
	Date    string `json:"date,omitempty"`
	Author  string `json:"author,omitempty"`
	Brand   bool   `json:"from_brand,omitempty"`
	Helpful int    `json:"helpful_votes,omitempty"`
}

type questionOut struct {
	Question string      `json:"question"`
	Details  string      `json:"details,omitempty"`
	Date     string      `json:"date,omitempty"`
	Answers  []answerOut `json:"answers"`
}

type getReviewsOut struct {
	ItemNumber string `json:"item_number"`
	// ReviewCount and AverageRating cover every review, including ratings
	// given without text, which are not listed.
	ReviewCount     int            `json:"review_count"`
	AverageRating   *float64       `json:"average_rating,omitempty"`
	Distribution    map[string]int `json:"rating_distribution,omitempty"`
	RecommendedPct  *int           `json:"recommended_percent,omitempty"`
	Sort            string         `json:"sort"`
	Reviews         []reviewOut    `json:"reviews"`
	QuestionCount   int            `json:"question_count"`
	Questions       []questionOut  `json:"questions"`
	Source          string         `json:"source"`
	Note            string         `json:"note,omitempty"`
	QuestionsFailed string         `json:"questions_error,omitempty"`
}

func registerGetReviews(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_reviews",
		Description: "Customer reviews and questions-and-answers for one product, as shown on its Bunnings page: " +
			"rating breakdown, then written reviews (rating, title, text, date, helpful votes), most helpful first by default; " +
			"sort='lowest' surfaces the problems people hit. Questions with their answers come too, though few products have any. " +
			"Use it when comparing products or when the user asks what people think. Quote reviewers as reviewers, not as fact, " +
			"and say how many reviews an opinion rests on.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getReviewsIn) (*mcp.CallToolResult, getReviewsOut, error) {
		item := strings.TrimSpace(in.ItemNumber)
		if !bunnings.ValidItemNumber(item) {
			return nil, getReviewsOut{}, fmt.Errorf("item_number must be the digits from search_products, e.g. 3129102")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = defaultReviews
		}
		limit = min(limit, maxReviews)
		sort := bunnings.ReviewSort(strings.ToLower(strings.TrimSpace(in.Sort)))
		if sort == "" {
			sort = bunnings.SortHelpful
		}
		reviews, stats, err := d.API.Reviews(ctx, item, sort, limit)
		if err != nil {
			return nil, getReviewsOut{}, err
		}
		// Bazaarvoice does not rank syndicated reviews by helpful votes: for
		// one printer ink, all syndicated from the maker's site, the "most
		// helpful" were 0-vote reviews while 91 others had votes. Ask again for an
		// order it does honour, and say which order that is.
		var note string
		if sort == bunnings.SortHelpful && len(reviews) > 0 && !slices.ContainsFunc(reviews, func(r bunnings.Review) bool { return r.SyndicatedFrom == "" }) {
			from := reviews[0].SyndicatedFrom
			if recent, _, err := d.API.Reviews(ctx, item, bunnings.SortRecent, limit); err == nil {
				reviews, sort = recent, bunnings.SortRecent
				note = "These reviews are syndicated from " + from + ", which Bazaarvoice does not rank by helpful votes, so these are the newest instead."
			} else {
				note = "These reviews are syndicated from " + from + ", which Bazaarvoice does not rank by helpful votes, so they are not the most helpful."
			}
		}
		out := getReviewsOut{
			ItemNumber: item, ReviewCount: stats.Count, Sort: string(sort),
			Reviews: []reviewOut{}, Questions: []questionOut{},
			Source: "Bazaarvoice, the reviews service on Bunnings' product pages",
		}
		if stats.Count > 0 {
			avg := math.Round(stats.Average*100) / 100
			out.AverageRating = &avg
			out.Distribution = map[string]int{}
			for stars, n := range stats.Distribution {
				out.Distribution[fmt.Sprintf("%d_star", stars)] = n
			}
			if stats.RecommendOf > 0 {
				pct := int(math.Round(100 * float64(stats.Recommended) / float64(stats.RecommendOf)))
				out.RecommendedPct = &pct
			}
		}
		for _, r := range reviews {
			ro := reviewOut{
				Rating: r.Rating, Title: r.Title, Text: r.Text, Date: day(r.Submitted),
				Helpful: r.Helpful, NotHelpful: r.NotHelpful, Author: r.Author,
				Country: country(r.Locale), Recommends: r.Recommended, Syndicated: r.SyndicatedFrom,
			}
			if utf8.RuneCountInString(ro.Text) > maxReviewChars {
				ro.Text = string([]rune(ro.Text)[:maxReviewChars]) + "..."
				ro.Truncated = true
			}
			out.Reviews = append(out.Reviews, ro)
		}
		out.Note = note

		// Questions are a separate call that fails on its own: reviews
		// without them are still worth returning.
		qs, total, err := d.API.Questions(ctx, item, maxQuestions)
		if err != nil {
			out.QuestionsFailed = err.Error()
		}
		out.QuestionCount = total
		for _, q := range qs {
			qo := questionOut{Question: q.Summary, Details: q.Details, Date: day(q.Submitted), Answers: []answerOut{}}
			for _, a := range q.Answers {
				qo.Answers = append(qo.Answers, answerOut{a.Text, day(a.Submitted), a.Author, a.Brand, a.Helpful})
			}
			out.Questions = append(out.Questions, qo)
		}

		var b strings.Builder
		switch {
		case stats.Count == 0:
			out.Note = "No reviews for this product."
			fmt.Fprintf(&b, "%s: no reviews.", item)
		default:
			fmt.Fprintf(&b, "%s: %.1f/5 from %s", item, *out.AverageRating, plural(stats.Count, "review"))
			if out.RecommendedPct != nil {
				fmt.Fprintf(&b, ", %d%% would recommend", *out.RecommendedPct)
			}
			fmt.Fprintf(&b, ". Showing %d written", len(out.Reviews))
			if note == "" || sort != bunnings.SortHelpful {
				fmt.Fprintf(&b, ", %s first", map[bunnings.ReviewSort]string{
					bunnings.SortHelpful: "most helpful", bunnings.SortRecent: "newest",
					bunnings.SortLowest: "lowest rated", bunnings.SortHighest: "highest rated"}[sort])
			}
			b.WriteString(".")
			if len(out.Reviews) == 0 {
				out.Note = "Every review is a rating without text."
			}
			if out.Note != "" {
				b.WriteString(" " + out.Note)
			}
		}
		for _, r := range out.Reviews {
			when := ""
			if r.Date != "" {
				when = r.Date + ", "
			}
			fmt.Fprintf(&b, "\n- %d/5 %s (%s%d helpful): %s", r.Rating, r.Title, when, r.Helpful, snippet(r.Text, 160))
		}
		if total > 0 {
			fmt.Fprintf(&b, "\n%s:", plural(total, "customer question"))
			for _, q := range out.Questions {
				fmt.Fprintf(&b, "\n- Q: %s (%s)", snippet(q.Question, 160), plural(len(q.Answers), "answer"))
			}
		} else if out.QuestionsFailed == "" {
			b.WriteString("\nNo customer questions.")
		}
		return textAndJSON(b.String(), out), out, nil
	})
}

// day is a date, or nothing when Bazaarvoice gave none.
func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func country(locale string) string {
	if _, c, ok := strings.Cut(locale, "_"); ok {
		return c
	}
	return ""
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
