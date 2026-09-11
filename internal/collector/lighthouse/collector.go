package lighthouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// redactURLError strips the query string from a *url.Error before the error is
// wrapped and returned.
//
// The PSI API key travels as a ?key= query parameter, and Go's transport
// errors embed the full request URL in their message (`Get "https://…?key=…":
// dial tcp: …`). Without this, any transport-level failure — a DNS blip, a
// timeout, a reset connection — writes the key verbatim into the job's error
// column, from where GET /v1/jobs/{id} hands it to every API caller.
func redactURLError(err error) error {
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return err
	}
	redacted := "<redacted>"
	if u, perr := url.Parse(uerr.URL); perr == nil {
		u.RawQuery = ""
		u.User = nil
		redacted = u.String() + "?<redacted>"
	}
	return &url.Error{Op: uerr.Op, URL: redacted, Err: uerr.Err}
}

// Result represents the metrics collected from Google PageSpeed Insights.
type Result struct {
	Performance   float64 `json:"performance"`
	Accessibility float64 `json:"accessibility"`
	BestPractices float64 `json:"best_practices"`
	SEO           float64 `json:"seo"`
	PWA           float64 `json:"pwa,omitempty"`
	FetchTime     string  `json:"fetch_time"`
	LighthouseVer string  `json:"lighthouse_version"`
}

var (
	psiEndpoint   = "https://www.googleapis.com/pagespeedonline/v5/runPagespeed"
	psiEndpointMu sync.RWMutex
)

// SetEndpoint overrides the PSI API endpoint (used for testing). Safe to call
// concurrently with Collect.
func SetEndpoint(endpoint string) {
	psiEndpointMu.Lock()
	psiEndpoint = endpoint
	psiEndpointMu.Unlock()
}

func currentEndpoint() string {
	psiEndpointMu.RLock()
	defer psiEndpointMu.RUnlock()
	return psiEndpoint
}

// Collect performs a Lighthouse analysis via the PageSpeed Insights API.
func Collect(ctx context.Context, targetURL string, apiKey string) (*Result, error) {
	u, err := url.Parse(currentEndpoint())
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("url", targetURL)
	q.Set("category", "performance")
	q.Add("category", "accessibility")
	q.Add("category", "best-practices")
	q.Add("category", "seo")
	q.Add("category", "pwa")

	if apiKey != "" {
		q.Set("key", apiKey)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", redactURLError(err))
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("psi api request failed: %w", redactURLError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("psi api returned status %d", resp.StatusCode)
	}

	var data struct {
		LighthouseResult struct {
			LighthouseVersion string `json:"lighthouseVersion"`
			FetchTime         string `json:"fetchTime"`
			Categories        map[string]struct {
				Score float64 `json:"score"`
			} `json:"categories"`
		} `json:"lighthouseResult"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode psi response: %w", err)
	}

	res := &Result{
		LighthouseVer: data.LighthouseResult.LighthouseVersion,
		FetchTime:     data.LighthouseResult.FetchTime,
	}

	if c, ok := data.LighthouseResult.Categories["performance"]; ok {
		res.Performance = c.Score
	}
	if c, ok := data.LighthouseResult.Categories["accessibility"]; ok {
		res.Accessibility = c.Score
	}
	if c, ok := data.LighthouseResult.Categories["best-practices"]; ok {
		res.BestPractices = c.Score
	}
	if c, ok := data.LighthouseResult.Categories["seo"]; ok {
		res.SEO = c.Score
	}
	if c, ok := data.LighthouseResult.Categories["pwa"]; ok {
		res.PWA = c.Score
	}

	return res, nil
}
