package nhl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	DefaultBaseURL      = "https://api-web.nhle.com"
	DefaultStatsBaseURL = "https://api.nhle.com"
)

type Client struct {
	BaseURL      string
	StatsBaseURL string
	HTTP         *http.Client
}

func New() *Client {
	return &Client{
		BaseURL:      DefaultBaseURL,
		StatsBaseURL: DefaultStatsBaseURL,
		HTTP:         &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) Schedule(ctx context.Context, date string) (*ScheduleResponse, []byte, error) {
	raw, err := c.get(ctx, fmt.Sprintf("%s/v1/schedule/%s", c.BaseURL, date))
	if err != nil {
		return nil, nil, err
	}
	var s ScheduleResponse
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, fmt.Errorf("parse schedule: %w", err)
	}
	return &s, raw, nil
}
