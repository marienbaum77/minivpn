package check

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Reputation is how picky services see the node's IP. Each check is an HTTP
// request through the tunnel, so the services see exactly what the user's
// browser will look like.
type Reputation struct {
	Country string `json:"country"` // from ip-api; RU means Gemini and others refuse
	ISP     string `json:"isp"`
	// Proxy: ip-api lists the address as a known proxy or VPN.
	Proxy bool `json:"proxy"`
	// RedditOK: Reddit serves pages without login (it blocks many datacenters).
	RedditOK bool `json:"reddit_ok"`
	// OpenAIOK: the API does not answer "unsupported_country".
	OpenAIOK bool `json:"openai_ok"`
	// GoogleCaptcha: Google search demands a captcha, Gemini will likely too.
	GoogleCaptcha bool `json:"google_captcha"`
}

// Clean is what the panel shows as "IP is fine".
func (r Reputation) Clean() bool {
	return r.Country != "" && r.Country != "RU" && !r.Proxy && r.RedditOK && r.OpenAIOK && !r.GoogleCaptcha
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// CheckReputation queries the services through c. Unreachable services count
// as failures: from the user's side there is no difference.
func CheckReputation(ctx context.Context, c *http.Client) Reputation {
	var r Reputation
	if body, _, err := get(ctx, c, "http://ip-api.com/json/?fields=status,countryCode,isp,proxy"); err == nil {
		var v struct {
			Status, CountryCode, ISP string
			Proxy                    bool
		}
		if json.Unmarshal(body, &v) == nil && v.Status == "success" {
			r.Country, r.ISP, r.Proxy = v.CountryCode, v.ISP, v.Proxy
		}
	}
	if _, code, err := get(ctx, c, "https://www.reddit.com/r/popular/"); err == nil {
		r.RedditOK = code == http.StatusOK
	}
	// Without a key the API answers 401 wherever it works and names the
	// region where it does not.
	if body, _, err := get(ctx, c, "https://api.openai.com/v1/models"); err == nil {
		r.OpenAIOK = !strings.Contains(string(body), "unsupported_country")
	}
	if body, code, err := get(ctx, c, "https://www.google.com/search?q=ip"); err == nil {
		r.GoogleCaptcha = code == http.StatusTooManyRequests || strings.Contains(string(body), "/sorry/") || strings.Contains(string(body), "unusual traffic")
	}
	return r
}

func get(ctx context.Context, c *http.Client, url string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}
