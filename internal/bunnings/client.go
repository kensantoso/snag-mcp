// Package bunnings is a client for the internal APIs behind bunnings.com.au.
//
// They are the same calls the website makes from a browser, authorised with the
// guest token any anonymous visitor gets. Nothing here needs an account or a
// key: the gateway client id is public, published in the site's own runtime
// config.
//
// Two rules hold for every call, because the server is someone else's:
//
//   - Concurrency is bounded process-wide. A shopping list against fifty stores
//     is hundreds of requests, and they queue here rather than arriving at once.
//   - There is no retry loop. A 401 refreshes the token and tries once more,
//     because guest tokens expire; anything else is reported, not repeated.
package bunnings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// UserAgent is a current desktop Chrome. The gateway sits behind a bot
	// filter that turns away obvious HTTP libraries.
	UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

	// gatewayClientID is APIGEE_CLIENT_ID from the website's
	// __NEXT_DATA__.runtimeConfig. Without it every call is a 401 SYS-3003.
	gatewayClientID = "mHPVWnzuBkrW7rmt56XGwKkb5Gp9BJMk"

	guestRedirect = "https://www.bunnings.com.au/static/guest.html"
	guestScope    = "chk:exec cm:access ecom:access chk:pub vch:public bsk:pub"
)

// Client talks to the Bunnings gateway. The zero value is not usable; call New.
type Client struct {
	// BaseURL is the gateway root, https://www.bunnings.com.au/_apis.
	BaseURL string
	// AuthURL is the guest authorize endpoint.
	AuthURL string
	// BVConfigURL and BVBaseURL are Bazaarvoice's widget config and API,
	// where reviews and questions live.
	BVConfigURL string
	BVBaseURL   string
	// MapBaseURL is Bunnings' store map viewer; MapCacheDir is where its
	// floor plans are kept between runs ("" keeps nothing on disk).
	MapBaseURL  string
	MapCacheDir string
	HTTP        *http.Client

	sem chan struct{}

	mu          sync.Mutex
	token       string
	expires     time.Time
	bvKeyCached string
}

// New returns a client allowing at most concurrency requests in flight at once.
func New(concurrency int) *Client {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Client{
		BaseURL:     "https://www.bunnings.com.au/_apis",
		AuthURL:     "https://authorisation.api.bunnings.com.au/connect/authorize",
		BVConfigURL: bvConfigURL,
		BVBaseURL:   bvAPI,
		MapBaseURL:  mapViewer,
		MapCacheDir: DefaultMapCacheDir(),
		HTTP:        &http.Client{Timeout: 20 * time.Second},
		sem:         make(chan struct{}, concurrency),
	}
}

// APIError is a gateway response whose statusDetails.state is not SUCCESS, or a
// non-2xx status.
type APIError struct {
	Status      int
	Code        string
	Description string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("bunnings: %s %s (HTTP %d)", e.Code, e.Description, e.Status)
	}
	return fmt.Sprintf("bunnings: HTTP %d %s", e.Status, e.Description)
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// guestToken returns a cached token, fetching a new one when it is missing or
// close to expiry.
func (c *Client) guestToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	tok, ttl, err := c.fetchGuestToken(ctx)
	if err != nil {
		return "", err
	}
	// Refresh an hour early: a token that expires mid-list would turn half a
	// fan-out into 401s.
	c.token, c.expires = tok, time.Now().Add(ttl-time.Hour)
	return tok, nil
}

func (c *Client) invalidate(tok string) {
	c.mu.Lock()
	if c.token == tok {
		c.token = ""
	}
	c.mu.Unlock()
}

// fetchGuestToken runs the implicit-grant flow the website runs for an
// anonymous visitor. It bounces through /Account/Login, which sets a cookie the
// callback needs, so the redirects must share a jar. The token arrives in the
// fragment of the final redirect to guest.html, which is read from the
// Location header rather than followed.
func (c *Client) fetchGuestToken(ctx context.Context) (string, time.Duration, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Timeout:   c.HTTP.Timeout,
		Transport: c.HTTP.Transport,
		Jar:       jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if strings.HasPrefix(req.URL.String(), guestRedirect) {
				return http.ErrUseLastResponse
			}
			if len(via) > 10 {
				return errors.New("too many redirects")
			}
			req.Header.Set("User-Agent", UserAgent)
			return nil
		},
	}
	// Parameter order matters. In url.Values' alphabetical order (acr_values
	// first) the login challenge answers 401; in the website's order it works.
	var q strings.Builder
	for i, kv := range [][2]string{
		{"response_type", "token"},
		{"client_id", "budp_guest_user_au"},
		{"scope", guestScope},
		{"redirect_uri", guestRedirect},
		{"nonce", randomHex(16)},
		{"acr_values", "adtid:" + randomUUID()},
	} {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString(kv[0] + "=" + url.QueryEscape(kv[1]))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AuthURL+"?"+q.String(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", UserAgent)
	c.sem <- struct{}{}
	resp, err := hc.Do(req)
	<-c.sem
	if err != nil {
		return "", 0, fmt.Errorf("bunnings: guest token: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	i := strings.IndexByte(loc, '#')
	if !strings.HasPrefix(loc, guestRedirect) || i < 0 {
		return "", 0, fmt.Errorf("bunnings: guest token: unexpected end of login flow (HTTP %d, %q)", resp.StatusCode, truncate(loc, 120))
	}
	frag, err := url.ParseQuery(loc[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bunnings: guest token: %w", err)
	}
	tok := frag.Get("access_token")
	if tok == "" {
		return "", 0, errors.New("bunnings: guest token: no access_token in redirect")
	}
	ttl := 5 * 24 * time.Hour
	var secs int
	if _, err := fmt.Sscan(frag.Get("expires_in"), &secs); err == nil && secs > 7200 {
		ttl = time.Duration(secs) * time.Second
	}
	return tok, ttl, nil
}

// call performs one gateway request and decodes data into out.
//
// store becomes the locationCode header, which the gateway uses for anything
// store-specific; region, when set, becomes X-region, which search needs.
func (c *Client) call(ctx context.Context, method, path, store, region string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.guestToken(ctx)
		if err != nil {
			return err
		}
		status, raw, err := c.send(ctx, method, path, store, region, tok, payload)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.invalidate(tok)
			continue
		}
		return decodeEnvelope(status, raw, out)
	}
}

func (c *Client) send(ctx context.Context, method, path, store, region, tok string, payload []byte) (int, []byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+tok)
	h.Set("clientId", gatewayClientID)
	h.Set("country", "AU")
	h.Set("currency", "AUD")
	h.Set("locale", "en_AU")
	h.Set("correlationid", randomUUID())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", UserAgent)
	if store != "" {
		h.Set("locationCode", store)
	}
	if region != "" {
		h.Set("X-region", region)
	}

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	resp, err := c.HTTP.Do(req)
	<-c.sem
	if err != nil {
		return 0, nil, fmt.Errorf("bunnings: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("bunnings: %s %s: %w", method, path, err)
	}
	return resp.StatusCode, raw, nil
}

// decodeEnvelope unwraps {"statusDetails":{...},"data":...}.
func decodeEnvelope(status int, raw []byte, out any) error {
	var env struct {
		StatusDetails struct {
			State       string `json:"state"`
			ErrorCode   string `json:"errorCode"`
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"statusDetails"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		if status/100 != 2 {
			return &APIError{Status: status, Description: truncate(string(raw), 200)}
		}
		return fmt.Errorf("bunnings: undecodable response: %w", err)
	}
	sd := env.StatusDetails
	if status/100 != 2 || (sd.State != "" && sd.State != "SUCCESS") {
		code := sd.ErrorCode
		if code == "" {
			code = sd.Code
		}
		return &APIError{Status: status, Code: code, Description: sd.Description}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
