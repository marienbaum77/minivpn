package check

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// routeByHost answers every request from h, whatever the URL's host.
type routeByHost struct{ h http.Handler }

func (r routeByHost) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func TestCheckReputation(t *testing.T) {
	answers := map[string]struct {
		code int
		body string
	}{
		"ip-api.com":     {200, `{"status":"success","countryCode":"US","isp":"ITGLOBAL","proxy":false}`},
		"www.reddit.com": {403, `blocked`},
		"api.openai.com": {401, `{"error":{"code":"invalid_api_key"}}`},
		"www.google.com": {200, `<html>results</html>`},
	}
	c := &http.Client{Transport: routeByHost{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answers[r.Host]
		w.WriteHeader(a.code)
		w.Write([]byte(a.body))
	})}}

	got := CheckReputation(context.Background(), c)
	want := Reputation{Country: "US", ISP: "ITGLOBAL", RedditOK: false, OpenAIOK: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.Clean() {
		t.Fatal("Reddit refused, so the IP is not clean")
	}
}
