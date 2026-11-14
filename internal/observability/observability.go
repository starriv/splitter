// Package observability reads the splitter's own Prometheus series back from a
// Prometheus server and assembles the control console's observability page.
//
// The page reproduces the splitter panels of the Grafana "DNS 解析与分流"
// dashboard. Prometheus is the only component that keeps history, so the
// console queries it instead of keeping a second time-series store in the
// resolver. The feature is disabled unless observability.prometheus_url is set;
// in that case no network request is made and the page only explains how to
// enable it.
//
// Only the Prometheus HTTP API is used, and only through the two read
// endpoints /api/v1/query and /api/v1/query_range. The response shape is
// normalized here so that the page receives aligned samples and cannot be
// confused by NaN, missing samples, or a changing label order.
package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultWindow is the range the page opens with, matching the Grafana
	// dashboard link (from=now-6h).
	DefaultWindow = 6 * time.Hour
	// MinWindow and MaxWindow bound the range accepted from the page.
	MinWindow = 5 * time.Minute
	MaxWindow = 30 * 24 * time.Hour

	// targetPoints is the number of samples a chart should have. The step is
	// chosen from nice durations so that consecutive points stay readable.
	targetPoints = 360
	// snapshotTimeout bounds one whole page refresh. Nineteen panels share it;
	// a slow Prometheus yields per-panel errors instead of a hanging request.
	snapshotTimeout = 20 * time.Second
	// maxResponseBytes caps one Prometheus response. The values are small, but
	// a misconfigured server could stream a large body.
	maxResponseBytes = 16 << 20
	// maxSeries and maxPoints bound one result in case a query drops its
	// aggregation.
	maxSeries = 500
	maxPoints = 5000
	// queryConcurrency bounds the parallel Prometheus requests of one refresh.
	queryConcurrency = 6
)

// Settings is the observability block after defaults were applied. It is built
// from config.Config by the app.
type Settings struct {
	URL          string        // Prometheus base URL; empty disables the page
	Job          string        // scrape job name used by the scrape-health panel
	DashboardURL string        // optional Grafana dashboard link
	Timeout      time.Duration // deadline for one Prometheus request
}

// Client queries one Prometheus server. It is immutable after New and safe for
// concurrent use. A nil *Client is a disabled client: Snapshot reports that
// Prometheus is not configured and makes no request.
type Client struct {
	base      *url.URL
	http      *http.Client
	job       string
	dashboard string
}

// New returns a client for s, or nil when s.URL is empty. The URL is validated
// by package config; a URL that cannot be parsed also disables the feature
// rather than failing a reload of the resolver.
func New(s Settings) *Client {
	if strings.TrimSpace(s.URL) == "" {
		return nil
	}
	base, err := url.Parse(s.URL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil
	}
	if s.Job == "" {
		s.Job = "splitter"
	}
	if s.Timeout <= 0 {
		s.Timeout = 8 * time.Second
	}
	return &Client{
		base:      base,
		http:      &http.Client{Timeout: s.Timeout},
		job:       s.Job,
		dashboard: s.DashboardURL,
	}
}

// ParseWindow validates the page's range parameter. An empty value selects the
// dashboard default so that the page works without knowing our presets.
func ParseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultWindow, nil
	}
	d, err := parseWindowDuration(s)
	if err != nil {
		return 0, fmt.Errorf("range %q: want a duration such as 6h, 3d or 2w", s)
	}
	if d < MinWindow || d > MaxWindow {
		return 0, fmt.Errorf("range %q outside [%s, %s]", s, MinWindow, MaxWindow)
	}
	return d, nil
}

// parseWindowDuration parses a Go duration and, additionally, whole days and
// weeks ("3d", "2w"), which are how people describe dashboard ranges.
func parseWindowDuration(s string) (time.Duration, error) {
	if len(s) > 1 {
		switch s[len(s)-1] {
		case 'd', 'w':
			mult := 24 * time.Hour
			if s[len(s)-1] == 'w' {
				mult = 7 * 24 * time.Hour
			}
			n, err := strconv.ParseFloat(s[:len(s)-1], 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid number in %q", s)
			}
			return time.Duration(n * float64(mult)), nil
		}
	}
	return time.ParseDuration(s)
}

// stepFor picks the sample interval for a window: the smallest nice duration
// that keeps a chart near targetPoints samples.
func stepFor(window time.Duration) time.Duration {
	nice := []time.Duration{
		15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
		10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour,
		6 * time.Hour,
	}
	want := window / targetPoints
	for _, d := range nice {
		if d >= want {
			return d
		}
	}
	return nice[len(nice)-1]
}

// vector is one instant sample with its label set.
type vector struct {
	labels map[string]string
	value  float64
}

// point is one range sample, in milliseconds since the Unix epoch.
type point struct {
	at    int64
	value float64
}

// matrix is one range series with its label set.
type matrix struct {
	labels map[string]string
	points []point
}

// query runs an instant query at at.
func (c *Client) query(ctx context.Context, expr string, at time.Time) ([]vector, error) {
	data, err := c.get(ctx, "api/v1/query", url.Values{
		"query": {expr},
		"time":  {formatTimestamp(at)},
	})
	if err != nil {
		return nil, err
	}
	var result struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  sampleValue       `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode instant query: %w", err)
	}
	if result.ResultType != "vector" {
		return nil, fmt.Errorf("instant query returned %q, want a vector", result.ResultType)
	}
	if len(result.Result) > maxSeries {
		return nil, fmt.Errorf("instant query returned %d series (limit %d); aggregate the query", len(result.Result), maxSeries)
	}
	out := make([]vector, 0, len(result.Result))
	for _, r := range result.Result {
		_, v, ok := r.Value.parse()
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, vector{labels: r.Metric, value: v})
	}
	return out, nil
}

// queryRange runs a range query between start and end with the given step.
func (c *Client) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]matrix, error) {
	data, err := c.get(ctx, "api/v1/query_range", url.Values{
		"query": {expr},
		"start": {formatTimestamp(start)},
		"end":   {formatTimestamp(end)},
		"step":  {formatStep(step)},
	})
	if err != nil {
		return nil, err
	}
	var result struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values []sampleValue     `json:"values"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode range query: %w", err)
	}
	if result.ResultType != "matrix" {
		return nil, fmt.Errorf("range query returned %q, want a matrix", result.ResultType)
	}
	if len(result.Result) > maxSeries {
		return nil, fmt.Errorf("range query returned %d series (limit %d); aggregate the query", len(result.Result), maxSeries)
	}
	out := make([]matrix, 0, len(result.Result))
	for _, r := range result.Result {
		if len(r.Values) > maxPoints {
			return nil, fmt.Errorf("range query returned %d points (limit %d); use a larger step", len(r.Values), maxPoints)
		}
		m := matrix{labels: r.Metric, points: make([]point, 0, len(r.Values))}
		for _, s := range r.Values {
			at, v, ok := s.parse()
			if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			m.points = append(m.points, point{at: at.UnixMilli(), value: v})
		}
		out = append(out, m)
	}
	return out, nil
}

// get performs one Prometheus API GET and returns the "data" member.
func (c *Client) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	u := c.base.JoinPath(path)
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "splitter-observability")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", u.Redacted(), err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response from %s exceeds %d bytes", u.Redacted(), maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus at %s answered %s", u.Redacted(), resp.Status)
	}
	var envelope struct {
		Status    string          `json:"status"`
		ErrorType string          `json:"errorType"`
		Error     string          `json:"error"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode response from %s: %w", u.Redacted(), err)
	}
	if envelope.Status != "success" {
		msg := strings.TrimSpace(envelope.Error)
		if msg == "" {
			msg = fmt.Sprintf("status %q", envelope.Status)
		} else if envelope.ErrorType != "" {
			msg = envelope.ErrorType + ": " + msg
		}
		return nil, fmt.Errorf("Prometheus at %s rejected the query: %s", u.Redacted(), msg)
	}
	if len(envelope.Data) == 0 {
		return nil, errors.New("Prometheus response has no data member")
	}
	return envelope.Data, nil
}

// sampleValue is the Prometheus [timestamp, "value"] pair. The timestamp is a
// JSON number and the value a JSON string; both are decoded explicitly so that
// "NaN", "+Inf" and "-Inf" are handled here rather than reaching the page.
type sampleValue struct {
	at    json.RawMessage
	value json.RawMessage
}

// UnmarshalJSON implements json.Unmarshaler for the two-element sample array.
func (s *sampleValue) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("sample has %d elements, want 2", len(pair))
	}
	s.at, s.value = pair[0], pair[1]
	return nil
}

// parse decodes one sample. The boolean is false for a shape that is not a
// number pair, so a single odd sample does not fail the whole panel.
func (s sampleValue) parse() (time.Time, float64, bool) {
	var seconds float64
	if err := json.Unmarshal(s.at, &seconds); err != nil {
		return time.Time{}, 0, false
	}
	var text string
	if err := json.Unmarshal(s.value, &text); err != nil {
		var number float64
		if err := json.Unmarshal(s.value, &number); err != nil {
			return time.Time{}, 0, false
		}
		return time.UnixMilli(int64(math.Round(seconds * 1000))), number, true
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	return time.UnixMilli(int64(math.Round(seconds * 1000))), v, true
}

// formatTimestamp renders seconds since the epoch with millisecond precision,
// which the Prometheus API accepts.
func formatTimestamp(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

// formatStep renders a sample interval as a Prometheus duration.
func formatStep(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}
