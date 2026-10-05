// Package healthchecks implements the Healthchecks Management v3 and Pinging APIs.
package healthchecks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Spec struct {
	Name         string   `json:"name"`
	Slug         string   `json:"slug"`
	Tags         string   `json:"tags"`
	Description  string   `json:"desc"`
	Schedule     string   `json:"schedule"`
	Timezone     string   `json:"tz"`
	Grace        int      `json:"grace"`
	Channels     string   `json:"channels"`
	ManualResume bool     `json:"manual_resume"`
	Unique       []string `json:"unique,omitempty"`
}

type Check struct {
	Spec
	UUID    string `json:"uuid"`
	PingURL string `json:"ping_url"`
	Status  string `json:"status"`
}

type RetryAfterError struct{ Delay time.Duration }

func (e *RetryAfterError) Error() string {
	return "Healthchecks ping deferred to respect per-check rate limit"
}

type Client struct {
	pingInterval time.Duration
	nextPing     map[string]time.Time
	base         string
	key          string
	http         *http.Client
	interval     time.Duration
	mu           sync.Mutex
	next         time.Time
}

func New(base, key string, timeout, interval time.Duration) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Healthchecks API URL")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("Healthchecks API key is required")
	}
	if timeout <= 0 || interval < 0 {
		return nil, errors.New("invalid HTTP timeout or request interval")
	}
	return &Client{pingInterval: 13 * time.Second, nextPing: make(map[string]time.Time), base: strings.TrimRight(base, "/"), key: key, interval: interval, http: &http.Client{
		Timeout: timeout,
		// Never forward management credentials or ping capabilities through redirects.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	c.next = at.Add(c.interval)
	c.mu.Unlock()
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return errors.New("cannot encode Healthchecks request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return errors.New("cannot construct Healthchecks request")
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "healthchecks-kubernetes")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("Healthchecks management request failed (connection, TLS, or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Response bodies and URLs can contain credentials or customer data.
		return fmt.Errorf("Healthchecks management API returned HTTP %d", resp.StatusCode)
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(output); err != nil {
			return errors.New("invalid Healthchecks management response")
		}
	}
	return nil
}

func (c *Client) List(ctx context.Context, tag string) ([]Check, error) {
	var result struct {
		Checks []Check `json:"checks"`
	}
	err := c.request(ctx, http.MethodGet, "/checks/?tag="+url.QueryEscape(tag), nil, &result)
	return result.Checks, err
}

func (c *Client) Upsert(ctx context.Context, spec Spec) (Check, error) {
	spec.Unique = []string{"slug"}
	var check Check
	err := c.request(ctx, http.MethodPost, "/checks/", spec, &check)
	if err != nil {
		return Check{}, err
	}
	if !uuidPattern.MatchString(check.UUID) || check.PingURL == "" {
		return Check{}, errors.New("Healthchecks response is missing a valid UUID or ping URL; use a read-write API key")
	}
	return check, nil
}

func (c *Client) Pause(ctx context.Context, id string) error  { return c.action(ctx, id, "pause") }
func (c *Client) Resume(ctx context.Context, id string) error { return c.action(ctx, id, "resume") }
func (c *Client) action(ctx context.Context, id, action string) error {
	if !uuidPattern.MatchString(id) {
		return errors.New("invalid check UUID")
	}
	return c.request(ctx, http.MethodPost, "/checks/"+id+"/"+action, nil, nil)
}

// Ping does not send the management API key. The returned ping URL is itself a secret.
func (c *Client) Ping(ctx context.Context, check Check, runID, signal string) error {
	if !uuidPattern.MatchString(runID) {
		return errors.New("invalid Job UID for Healthchecks run ID")
	}
	if signal != "start" && signal != "success" && signal != "fail" && signal != "initialize" {
		return errors.New("invalid ping signal")
	}
	u, err := url.Parse(check.PingURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid Healthchecks ping URL")
	}
	if signal != "success" && signal != "initialize" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + signal
	}
	query := u.Query()
	query.Set("rid", runID)
	u.RawQuery = query.Encode()
	var bodyReader io.Reader
	if signal == "initialize" {
		bodyReader = strings.NewReader("Monitoring enabled by healthchecks-kubernetes. This is an initialization signal, not a Job execution.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bodyReader)
	if err != nil {
		return errors.New("cannot construct Healthchecks ping")
	}
	req.Header.Set("User-Agent", "healthchecks-kubernetes")
	c.mu.Lock()
	now := time.Now()
	// Expire entries so removed checks do not accumulate in memory.
	for id, next := range c.nextPing {
		if !next.After(now) {
			delete(c.nextPing, id)
		}
	}
	if next := c.nextPing[check.PingURL]; next.After(now) {
		c.mu.Unlock()
		return &RetryAfterError{Delay: time.Until(next)}
	}
	c.nextPing[check.PingURL] = now.Add(c.pingInterval)
	c.mu.Unlock()
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("Healthchecks ping failed (connection, TLS, or timeout)")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return errors.New("cannot read Healthchecks ping response")
	}
	// UUID ping endpoints can return HTTP 200 with 'not found' or 'rate limited'.
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "OK" {
		return fmt.Errorf("Healthchecks did not accept ping (HTTP %d)", resp.StatusCode)
	}
	return nil
}
