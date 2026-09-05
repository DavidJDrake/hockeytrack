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

// UserAgent identifies this project to the NHL's operators, with a way to
// reach us, instead of Go's anonymous default.
const UserAgent = "hockeytrack/1.0 (+https://github.com/DavidJDrake/hockeytrack)"

// MaxResponseBytes caps how much of an NHL response body get() will buffer.
// The largest real feeds (play-by-play, shift charts) are a few hundred KB,
// so 16 MiB leaves ample headroom while keeping a runaway upstream from
// exhausting Lambda memory.
const MaxResponseBytes = 16 << 20

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, MaxResponseBytes)
	}
	return body, nil
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

func (c *Client) PlayByPlay(ctx context.Context, gameID int64) (*PlayByPlay, []byte, error) {
	raw, err := c.get(ctx, fmt.Sprintf("%s/v1/gamecenter/%d/play-by-play", c.BaseURL, gameID))
	if err != nil {
		return nil, nil, err
	}
	var p PlayByPlay
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("parse play-by-play: %w", err)
	}
	return &p, raw, nil
}

func (c *Client) RawFeed(ctx context.Context, gameID int64, feed string) ([]byte, error) {
	return c.get(ctx, fmt.Sprintf("%s/v1/gamecenter/%d/%s", c.BaseURL, gameID, feed))
}

func (c *Client) ShiftCharts(ctx context.Context, gameID int64) ([]byte, error) {
	return c.get(ctx, fmt.Sprintf("%s/stats/rest/en/shiftcharts?cayenneExp=gameId=%d", c.StatsBaseURL, gameID))
}
