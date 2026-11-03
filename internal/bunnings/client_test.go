package bunnings

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAuth mimics the guest login: the first hop sets a cookie that the
// callback insists on, then the token arrives in the fragment of a redirect to
// guest.html, which must not be followed.
func fakeAuth(t *testing.T, issued *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect/authorize":
			if !strings.HasPrefix(r.URL.RawQuery, "response_type=token&client_id=") {
				w.WriteHeader(http.StatusUnauthorized) // the real one is this fussy
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "idsrv", Value: "x", Path: "/"})
			http.Redirect(w, r, "/callback", http.StatusFound)
		case "/callback":
			if _, err := r.Cookie("idsrv"); err != nil {
				http.Redirect(w, r, "/home/error", http.StatusFound)
				return
			}
			n := issued.Add(1)
			http.Redirect(w, r, fmt.Sprintf("%s#access_token=TOK%d&token_type=Bearer&expires_in=432000", guestRedirect, n), http.StatusFound)
		default:
			t.Errorf("unexpected auth request %s", r.URL)
		}
	}))
}

func TestGuestTokenAndRefreshOn401(t *testing.T) {
	var issued atomic.Int32
	auth := fakeAuth(t, &issued)
	defer auth.Close()

	var calls atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("clientId") != gatewayClientID || r.Header.Get("locationCode") != "0101" {
			t.Errorf("missing gateway headers: %v", r.Header)
		}
		// The first token has "expired" server-side.
		if r.Header.Get("Authorization") == "Bearer TOK1" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"statusDetails":{"state":"FAILURE","errorCode":"SYS-3003"}}`)
			return
		}
		fmt.Fprint(w, `{"statusDetails":{"state":"SUCCESS"},"data":[{"inStoreLocations":[{"aisle":"22","bay":"2"}]}]}`)
	}))
	defer gw.Close()

	c := New(2)
	c.AuthURL, c.BaseURL = auth.URL+"/connect/authorize", gw.URL
	locs, err := c.Locations(context.Background(), "9100002", "0101")
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 1 || locs[0] != (Location{Aisle: "22", Bay: "2"}) {
		t.Errorf("locations = %v", locs)
	}
	if issued.Load() != 2 || calls.Load() != 2 {
		t.Errorf("tokens issued %d, gateway calls %d; want one refresh and one retry", issued.Load(), calls.Load())
	}
}

// A 401 that persists after a fresh token is reported, not retried forever.
func TestPersistent401IsNotRetriedAgain(t *testing.T) {
	var issued atomic.Int32
	auth := fakeAuth(t, &issued)
	defer auth.Close()
	var calls atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"statusDetails":{"state":"FAILURE","errorCode":"SYS-3003","description":"Invalid key"}}`)
	}))
	defer gw.Close()
	c := New(2)
	c.AuthURL, c.BaseURL = auth.URL+"/connect/authorize", gw.URL
	_, err := c.Product(context.Background(), "9100002", "0101")
	if !IsCode(err, "SYS-3003") || calls.Load() != 2 {
		t.Fatalf("err %v after %d calls; want SYS-3003 after 2", err, calls.Load())
	}
}

func TestNoStoresInRangeIsEmpty(t *testing.T) {
	var issued atomic.Int32
	auth := fakeAuth(t, &issued)
	defer auth.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"statusDetails":{"state":"FAILURE","errorCode":"BUS-4455","description":"No stores found within the given search radius and lat long"}}`)
	}))
	defer gw.Close()
	c := New(1)
	c.AuthURL, c.BaseURL = auth.URL+"/connect/authorize", gw.URL
	st, err := c.StoresNear(context.Background(), -25.9, 139.35, 50, 10)
	if err != nil || len(st) != 0 {
		t.Fatalf("stores %v, err %v; want none and no error", st, err)
	}
}

func TestItemNumbersAreValidated(t *testing.T) {
	c := New(1)
	c.BaseURL = "http://unused.invalid"
	for _, bad := range []string{"", "../stores", "9100002?x=1", "abc"} {
		if _, err := c.Fulfilment(context.Background(), bad, "0101"); err == nil || !strings.Contains(err.Error(), "invalid item number") {
			t.Errorf("Fulfilment(%q) err = %v", bad, err)
		}
	}
}

// The multi-store lookup reports some special orders as 9999 in stock. That
// is a placeholder, and passing it on would tell a shopper a shelf is full.
func TestStockNearDropsPlaceholderCount(t *testing.T) {
	var issued atomic.Int32
	auth := fakeAuth(t, &issued)
	defer auth.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"statusDetails":{"state":"SUCCESS"},"data":{"pagination":{"totalPages":1},"stores":[
			{"name":"6427","products":[
				{"code":"9100004","stock":{"stockLevel":9999},"visible":true},
				{"code":"9100001","stock":{"stockLevel":36},"visible":true},
				{"code":"9100003","stock":{"stockLevel":null},"visible":false}]}]}}`)
	}))
	defer gw.Close()
	c := New(1)
	c.AuthURL, c.BaseURL = auth.URL+"/connect/authorize", gw.URL
	got, err := c.StockNear(context.Background(), -38.1, 145.2, 50, []string{"9100004", "9100001", "9100003"})
	if err != nil {
		t.Fatal(err)
	}
	at := got["6427"]
	if at["9100004"].Level != nil {
		t.Errorf("placeholder 9999 reported as a count: %d", *at["9100004"].Level)
	}
	if l := at["9100001"].Level; l == nil || *l != 36 {
		t.Errorf("real count lost: %v", l)
	}
	if at["9100003"].Visible || at["9100003"].Level != nil {
		t.Errorf("unranged item = %+v", at["9100003"])
	}
}
