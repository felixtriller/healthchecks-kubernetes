package healthchecks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const checkID = "11111111-1111-4111-8111-111111111111"
const runID = "22222222-2222-4222-8222-222222222222"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func newTestClient(t *testing.T, transport transportFunc) *Client {
	t.Helper()
	c, err := New("https://example.test/api/v3/", "test-management-secret", time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = transport
	c.pingInterval = 0
	return c
}

func TestUpsert(t *testing.T) {
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://example.test/api/v3/checks/" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "test-management-secret" {
			t.Error("missing authentication")
		}
		var spec Spec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			t.Fatal(err)
		}
		if len(spec.Unique) != 1 || spec.Unique[0] != "slug" || spec.Slug != "k8s-stable" {
			t.Errorf("unexpected identity: %+v", spec)
		}
		if !spec.ManualResume || spec.Schedule != "*/5 * * * *" || spec.Channels != "*" {
			t.Errorf("missing settings: %+v", spec)
		}
		return response(201, `{"uuid":"`+checkID+`","ping_url":"https://ping.example.test/`+checkID+`","status":"new"}`), nil
	})
	check, err := c.Upsert(context.Background(), Spec{Slug: "k8s-stable", Schedule: "*/5 * * * *", Channels: "*", ManualResume: true})
	if err != nil {
		t.Fatal(err)
	}
	if check.UUID != checkID || check.Status != "new" {
		t.Fatalf("unexpected response: %+v", check)
	}
}

func TestPingSignals(t *testing.T) {
	for _, signal := range []string{"start", "success", "fail", "initialize"} {
		t.Run(signal, func(t *testing.T) {
			c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
				want := "/ping/" + checkID
				if signal == "start" || signal == "fail" {
					want += "/" + signal
				}
				if r.URL.Path != want || r.URL.Query().Get("rid") != runID || r.Method != "POST" {
					t.Errorf("incorrect ping: %s %s", r.Method, r.URL.String())
				}
				if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "" {
					t.Error("management credentials leaked to ping endpoint")
				}
				if signal == "initialize" {
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), "not a Job execution") {
						t.Error("initialization must be labeled")
					}
				}
				return response(200, "OK"), nil
			})
			if err := c.Ping(context.Background(), Check{PingURL: "https://ping.example.test/ping/" + checkID}, runID, signal); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPingRejectsSilentFailures(t *testing.T) {
	for _, body := range []string{"OK (not found)", "OK (rate limited)", "not found", "<html>login</html>", ""} {
		t.Run(body, func(t *testing.T) {
			c := newTestClient(t, func(_ *http.Request) (*http.Response, error) { return response(200, body), nil })
			if err := c.Ping(context.Background(), Check{PingURL: "https://ping.example.test/" + checkID}, runID, "success"); err == nil {
				t.Fatal("rejected ping was treated as delivered")
			}
		})
	}
}

func TestSecretsNeverAppearInErrors(t *testing.T) {
	cases := []transportFunc{
		func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("failed URL https://ping.example.test/SECRET")
		},
		func(_ *http.Request) (*http.Response, error) { return response(500, "SECRET"), nil },
		func(_ *http.Request) (*http.Response, error) { return response(200, "SECRET"), nil },
	}
	for _, transport := range cases {
		c := newTestClient(t, transport)
		_, err := c.List(context.Background(), "owned")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("unsafe management error: %v", err)
		}
		err = c.Ping(context.Background(), Check{PingURL: "https://ping.example.test/SECRET"}, runID, "success")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("unsafe ping error: %v", err)
		}
	}
}

func TestManagementActionsAndList(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			if r.URL.Query().Get("tag") != "owner:one two" {
				t.Error("tag was not URL encoded")
			}
			return response(200, `{"checks":[{"uuid":"`+checkID+`","status":"paused"}]}`), nil
		case 2:
			if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/pause") {
				t.Error("bad pause request")
			}
		case 3:
			if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/resume") {
				t.Error("bad resume request")
			}
		}
		return response(200, `{}`), nil
	})
	checks, err := c.List(context.Background(), "owner:one two")
	if err != nil || len(checks) != 1 || checks[0].UUID != checkID {
		t.Fatalf("list failed: %v", err)
	}
	if err := c.Pause(context.Background(), checkID); err != nil {
		t.Fatal(err)
	}
	if err := c.Resume(context.Background(), checkID); err != nil {
		t.Fatal(err)
	}
	if err := c.Pause(context.Background(), "../bad"); err == nil {
		t.Fatal("invalid UUID accepted")
	}
	if calls != 3 {
		t.Fatalf("got %d calls", calls)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		result := response(302, "")
		result.Header.Set("Location", "https://different.example.test/collect")
		return result, nil
	})
	if _, err := c.List(context.Background(), "owner"); err == nil {
		t.Fatal("redirect accepted")
	}
	if err := c.Ping(context.Background(), Check{PingURL: "https://ping.example.test/" + checkID}, runID, "start"); err == nil {
		t.Fatal("ping redirect accepted")
	}
	if calls != 2 {
		t.Fatalf("followed redirect: %d calls", calls)
	}
}

func TestValidationAndCancellation(t *testing.T) {
	for _, base := range []string{"not-a-url", "ftp://example.test", "https://user:pass@example.test/api/v3", "https://example.test/api/v3?key=secret"} {
		if _, err := New(base, "key", time.Second, 0); err == nil {
			t.Errorf("accepted invalid base %q", base)
		}
	}
	c := newTestClient(t, func(_ *http.Request) (*http.Response, error) { t.Fatal("unexpected HTTP request"); return nil, nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.next = time.Now().Add(time.Hour)
	if _, err := c.List(ctx, "owner"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if err := c.Ping(context.Background(), Check{}, "bad-id", "start"); err == nil {
		t.Fatal("invalid run ID accepted")
	}
	if err := c.Ping(context.Background(), Check{}, runID, "invalid"); err == nil {
		t.Fatal("invalid signal accepted")
	}
}

func TestUpsertRequiresWritableResponse(t *testing.T) {
	c := newTestClient(t, func(_ *http.Request) (*http.Response, error) { return response(200, `{"name":"readonly"}`), nil })
	if _, err := c.Upsert(context.Background(), Spec{}); err == nil {
		t.Fatal("missing UUID and ping URL accepted")
	}
}

func TestPingRateLimitDefersWithoutSending(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(_ *http.Request) (*http.Response, error) { calls++; return response(200, "OK"), nil })
	c.pingInterval = time.Minute
	check := Check{PingURL: "https://ping.example.test/" + checkID}
	if err := c.Ping(context.Background(), check, runID, "start"); err != nil {
		t.Fatal(err)
	}
	var deferred *RetryAfterError
	if err := c.Ping(context.Background(), check, runID, "success"); !errors.As(err, &deferred) || deferred.Delay <= 0 {
		t.Fatalf("expected deferral, got %v", err)
	}
	if calls != 1 {
		t.Fatal("rate limited ping was sent")
	}
	check.PingURL = "https://ping.example.test/another-check"
	if err := c.Ping(context.Background(), check, runID, "start"); err != nil {
		t.Fatal("another check was blocked", err)
	}
}
