package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// WaterfallEntry represents a single resource request in the timeline.
type WaterfallEntry struct {
	URL     string  `json:"url"`
	Type    string  `json:"type"`
	Status  int     `json:"status"`
	Size    int64   `json:"size_bytes"`
	TotalMS float64 `json:"total_ms"`
}

// Result represents the metrics collected from a headless browser.
type Result struct {
	DOMContentLoadedMS float64          `json:"dom_content_loaded_ms"`
	PageLoadMS         float64          `json:"page_load_ms"`
	ResourceCount      int              `json:"resource_count"`
	Waterfall          []WaterfallEntry `json:"waterfall,omitempty"`
}

// Collect performs a full page load analysis using headless Chrome.
func Collect(ctx context.Context, url string) (*Result, error) {
	var (
		res       Result
		startTime = time.Now()
		waterfall []WaterfallEntry
		mu        sync.Mutex
	)

	// Listen for network events to build waterfall
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventResponseReceived:
			mu.Lock()
			waterfall = append(waterfall, WaterfallEntry{
				URL:    ev.Response.URL,
				Type:   string(ev.Type),
				Status: int(ev.Response.Status),
				Size:   int64(ev.Response.EncodedDataLength),
			})
			mu.Unlock()
		}
	})

	// Perform navigation and extract timings via JS Performance API
	var timing map[string]float64
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1280, 800),
		network.Enable(),
		chromedp.Navigate(url),
		// Wait until the load event is definitely fired
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`(function() {
			const t = performance.getEntriesByType("navigation")[0];
			if (!t) return { "domContentLoaded": 0, "loadEventEnd": 0 };
			return {
				"domContentLoaded": t.domContentLoadedEventEnd,
				"loadEventEnd": t.loadEventEnd
			};
		})()`, &timing),
	)

	if err != nil {
		return nil, fmt.Errorf("chromedp run failed: %w", err)
	}

	res.DOMContentLoadedMS = timing["domContentLoaded"]
	res.PageLoadMS = timing["loadEventEnd"]

	// Snapshot under the lock the listener appends with: chromedp keeps
	// delivering network events on its own goroutine until the caller cancels
	// the context, which happens after Collect returns.
	mu.Lock()
	res.Waterfall = append([]WaterfallEntry(nil), waterfall...)
	res.ResourceCount = len(res.Waterfall)
	mu.Unlock()

	// In case performance API isn't fully ready (loadEventEnd is 0),
	// we fall back to our own timer as a sanity check.
	if res.PageLoadMS <= 0 {
		res.PageLoadMS = float64(time.Since(startTime).Nanoseconds()) / 1e6
	}

	return &res, nil
}
