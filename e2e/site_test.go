package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// site is a deliberately separate, minimal client for the Bunnings website's
// own APIs: the calls its product page makes, written again here so that the
// comparison does not check the server's code against itself. It uses
// priceInfo for price, where the server reads prices from search.
type site struct {
	token string
}

const siteUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

func newSite(t *testing.T) *site {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if r.URL.Host == "www.bunnings.com.au" {
			return http.ErrUseLastResponse
		}
		r.Header.Set("User-Agent", siteUA)
		return nil
	}}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	u := "https://authorisation.api.bunnings.com.au/connect/authorize?response_type=token&client_id=budp_guest_user_au" +
		"&scope=" + url.QueryEscape("chk:exec cm:access ecom:access chk:pub vch:public bsk:pub") +
		"&redirect_uri=" + url.QueryEscape("https://www.bunnings.com.au/static/guest.html") +
		"&nonce=" + hex.EncodeToString(nonce) + "&acr_values=" + url.QueryEscape("adtid:"+uuid())
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", siteUA)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	frag, _ := url.ParseQuery(strings.SplitN(resp.Header.Get("Location"), "#", 2)[1])
	if frag.Get("access_token") == "" {
		t.Fatalf("no guest token: %s", resp.Header.Get("Location"))
	}
	return &site{token: frag.Get("access_token")}
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (s *site) get(t *testing.T, method, path, store string, body any, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://www.bunnings.com.au/_apis"+path, rd)
	for k, v := range map[string]string{
		"Authorization": "Bearer " + s.token, "clientId": "mHPVWnzuBkrW7rmt56XGwKkb5Gp9BJMk",
		"locationCode": store, "country": "AU", "currency": "AUD", "locale": "en_AU",
		"correlationid": uuid(), "Content-Type": "application/json", "User-Agent": siteUA,
	} {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &env); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%s %s: HTTP %d %.200s", method, path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// siteView is what the website says about an item at a store.
type siteView struct {
	Price     float64
	Stock     *int
	Special   bool
	Active    bool
	Aisle     string
	Bay       string
	Formatted string
}

func (s *site) view(t *testing.T, item, store string) siteView {
	var price struct {
		Value     float64 `json:"value"`
		Formatted string  `json:"formattedValue"`
	}
	s.get(t, "GET", "/v2/products/"+item+"/priceInfo", store, nil, &price)
	var ful struct {
		IsActive  bool `json:"isActive"`
		IsSpecial bool `json:"isSpecialProduct"`
		InStore   struct {
			Stock *int `json:"stock"`
		} `json:"inStorePickUpData"`
	}
	s.get(t, "POST", "/v2/products/"+item+"/fulfillment", store,
		map[string]any{"includeVariantStock": true, "locationCode": store, "storeRadius": 20}, &ful)
	var locs []struct {
		InStoreLocations []struct{ Aisle, Bay string } `json:"inStoreLocations"`
	}
	s.get(t, "GET", fmt.Sprintf("/v1/item-api/locations?locationCode=%s&productCode=%s", store, item), store, nil, &locs)
	v := siteView{Price: price.Value, Formatted: price.Formatted, Stock: ful.InStore.Stock, Special: ful.IsSpecial, Active: ful.IsActive}
	if len(locs) > 0 && len(locs[0].InStoreLocations) > 0 {
		v.Aisle, v.Bay = locs[0].InStoreLocations[0].Aisle, locs[0].InStoreLocations[0].Bay
	}
	return v
}
