package apiengine

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func fastRetry() RetryConfig {
	return RetryConfig{MaxAttempts: 3, InitialBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond}
}

func testRemote(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestRetryStatuses(t *testing.T) {
	for _, status := range []int{200, 201, 204, 301, 400, 401, 403, 404, 408, 409, 422, 429, 500, 501, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			attempts := 0
			client := newTestClient(t, Config{Retry: fastRetry(), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				return testRemote(status, fmt.Sprint(attempts)), nil
			})}})
			response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com"})
			expected := 1
			switch status {
			case 408, 429, 500, 502, 503, 504:
				expected = 3
			}
			if err != nil || response == nil || response.StatusCode != status || string(response.Body) != fmt.Sprint(expected) || attempts != expected {
				t.Fatalf("attempts=%d expected=%d response=%+v error=%v", attempts, expected, response, err)
			}
		})
	}
}

func TestRetryMethodsAndIdempotency(t *testing.T) {
	for _, test := range []struct {
		name          string
		method        string
		optIn         bool
		defaultKey    string
		requestKey    string
		setRequestKey bool
		want          int
	}{
		{name: "GET", method: http.MethodGet, want: 3},
		{name: "PUT", method: http.MethodPut, want: 3},
		{name: "DELETE", method: http.MethodDelete, want: 3},
		{name: "POST/default", method: http.MethodPost, want: 1},
		{name: "POST/key without opt-in", method: http.MethodPost, requestKey: "key", setRequestKey: true, want: 1},
		{name: "POST/opt-in without key", method: http.MethodPost, optIn: true, want: 1},
		{name: "POST/blank key", method: http.MethodPost, optIn: true, requestKey: "  ", setRequestKey: true, want: 1},
		{name: "POST/opt-in and key", method: http.MethodPost, optIn: true, requestKey: "stable-key", setRequestKey: true, want: 3},
		{name: "POST/default header", method: http.MethodPost, optIn: true, defaultKey: "stable-key", want: 3},
		{name: "POST/override clears key", method: http.MethodPost, optIn: true, defaultKey: "key", setRequestKey: true, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			retry := fastRetry()
			retry.RetryPOSTWithIdempotencyKey = test.optIn
			client := newTestClient(t, Config{Retry: retry, Headers: http.Header{"Idempotency-Key": {test.defaultKey}}, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != `{"name":"resource"}` {
					t.Errorf("attempt %d: body=%q error=%v", attempts, body, err)
				}
				r.Body.Close()
				key := test.defaultKey
				if test.setRequestKey {
					key = test.requestKey
				}
				if r.Header.Get("Idempotency-Key") != key {
					t.Errorf("idempotency key changed on attempt %d", attempts)
				}
				if attempts == test.want {
					return testRemote(200, "ok"), nil
				}
				return testRemote(503, "retry"), nil
			})}})
			input := Request{Method: test.method, Endpoint: "https://example.com", Body: []byte(`{"name":"resource"}`)}
			if test.setRequestKey {
				input.Headers = http.Header{"idempotency-key": {test.requestKey}}
			}
			response, err := client.Do(context.Background(), input)
			if err != nil || response == nil || !response.IsSuccess() || attempts != test.want {
				t.Fatalf("attempts=%d response=%+v error=%v", attempts, response, err)
			}
		})
	}
}

func TestRetryTransportErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"EOF", io.EOF, 3},
		{"unexpected EOF", io.ErrUnexpectedEOF, 3},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 3},
		{"connection refused", syscall.ECONNREFUSED, 3},
		{"timeout", context.DeadlineExceeded, 3},
		{"canceled", context.Canceled, 1},
		{"certificate", x509.UnknownAuthorityError{}, 1},
		{"DNS not found", &net.DNSError{Err: "not found", Name: "example.invalid", IsNotFound: true}, 1},
		{"other", errors.New("permanent error"), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			client := newTestClient(t, Config{Retry: fastRetry(), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				attempts++
				return nil, test.err
			})}})
			response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com"})
			if response != nil || !errors.Is(err, test.err) || attempts != test.want {
				t.Fatalf("attempts=%d response=%+v error=%v", attempts, response, err)
			}
		})
	}
}

func TestRetryDisabled(t *testing.T) {
	for _, maxAttempts := range []int{0, 1} {
		attempts := 0
		client := newTestClient(t, Config{Retry: RetryConfig{MaxAttempts: maxAttempts}, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return testRemote(503, "unavailable"), nil
		})}})
		response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com"})
		if err != nil || response == nil || attempts != 1 {
			t.Fatalf("attempts=%d response=%+v error=%v", attempts, response, err)
		}
	}
}

func TestRetryDoesNotRepeatIncompleteResponses(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		attempts := 0
		body := &brokenBody{}
		client := newTestClient(t, Config{Retry: fastRetry(), MaxResponseBytes: 3, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			response := testRemote(503, "oversized")
			if !oversized {
				response.Body = body
			}
			return response, nil
		})}})
		if !oversized {
			client.maxResponseBytes = 100
		}
		response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com"})
		wantErr := io.ErrUnexpectedEOF
		if oversized {
			wantErr = ErrResponseTooLarge
		}
		if response == nil || !errors.Is(err, wantErr) || attempts != 1 || (!oversized && !body.closed) {
			t.Fatalf("attempts=%d response=%+v error=%v", attempts, response, err)
		}
	}
}

func TestRetryConfig(t *testing.T) {
	for _, retry := range []RetryConfig{
		{MaxAttempts: -1}, {InitialBackoff: -1}, {MaxBackoff: -1},
		{InitialBackoff: time.Second, MaxBackoff: time.Millisecond},
	} {
		if _, err := NewClient(Config{Retry: retry}); err == nil {
			t.Errorf("expected invalid retry config: %+v", retry)
		}
	}
	client := newTestClient(t, Config{Retry: RetryConfig{MaxAttempts: 3}})
	if client.retry.InitialBackoff != DefaultInitialBackoff || client.retry.MaxBackoff != DefaultMaxBackoff || client.retry.MaxAttempts != 3 {
		t.Fatalf("unexpected retry defaults: %+v", client.retry)
	}
}

func TestBackoffAndJitter(t *testing.T) {
	backoff := time.Second
	for _, expected := range []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		backoff = nextBackoff(backoff, 5*time.Second)
		if backoff != expected {
			t.Fatalf("backoff=%v expected=%v", backoff, expected)
		}
	}
	if nextBackoff(time.Duration(math.MaxInt64/2+1), time.Duration(math.MaxInt64)) != time.Duration(math.MaxInt64) {
		t.Fatal("backoff overflow")
	}
	for _, delay := range []time.Duration{0, 1, 3, time.Millisecond, time.Duration(math.MaxInt64)} {
		for i := 0; i < 100; i++ {
			if actual := jitter(delay); actual < delay/2 || actual > delay {
				t.Fatalf("jitter(%v)=%v", delay, actual)
			}
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0}, {"0", 0}, {" 2 ", 2 * time.Second},
		{"-1", 0}, {"+1", 0}, {"1.5", 0}, {"invalid", 0},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"9223372036854775807", time.Duration(math.MaxInt64)},
		{"999999999999999999999999999999", time.Duration(math.MaxInt64)},
	} {
		if actual := retryAfterDelay(test.value, now); actual != test.want {
			t.Errorf("Retry-After %q: %v expected %v", test.value, actual, test.want)
		}
	}
}

func TestRetryAfterWaitAndBodyReplay(t *testing.T) {
	var attempts atomic.Int32
	var mutex sync.Mutex
	var firstAttempt time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "payload" {
			t.Errorf("body=%q error=%v", body, err)
		}
		mutex.Lock()
		defer mutex.Unlock()
		if attempts.Add(1) == 1 {
			firstAttempt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			return
		}
		if time.Since(firstAttempt) < time.Second {
			t.Error("retried before Retry-After, or capped server delay by MaxBackoff")
		}
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := newTestClient(t, Config{Retry: fastRetry()}).Do(ctx, Request{Method: http.MethodPut, Endpoint: server.URL, Body: []byte("payload")})
	if err != nil || response == nil || !response.IsSuccess() || attempts.Load() != 2 {
		t.Fatalf("response=%+v error=%v attempts=%d", response, err, attempts.Load())
	}
}

func TestRetryWaitCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
		}
		attempts := 0
		client := newTestClient(t, Config{Retry: fastRetry(), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			if !deadline {
				cancel()
			}
			response := testRemote(429, "rate limited")
			response.Header.Set("Retry-After", "999999999999999999999999")
			return response, nil
		})}})
		response, err := client.Do(ctx, Request{Method: http.MethodGet, Endpoint: "https://example.com"})
		cancel()
		wantErr := context.Canceled
		if deadline {
			wantErr = context.DeadlineExceeded
		}
		if !errors.Is(err, wantErr) || response == nil || response.StatusCode != 429 || attempts != 1 {
			t.Fatalf("attempts=%d response=%+v error=%v", attempts, response, err)
		}
	}
}

func TestRetryPerAttemptTimeout(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "recovered")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := newTestClient(t, Config{Retry: fastRetry(), Timeout: 50 * time.Millisecond}).Do(ctx, Request{Method: http.MethodGet, Endpoint: server.URL})
	if err != nil || response == nil || string(response.Body) != "recovered" || attempts.Load() != 2 {
		t.Fatalf("response=%+v error=%v attempts=%d", response, err, attempts.Load())
	}
}

func TestConcurrentRetries(t *testing.T) {
	var counts sync.Map
	client := newTestClient(t, Config{Retry: fastRetry(), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		value, _ := counts.LoadOrStore(r.URL.Path, &atomic.Int32{})
		if value.(*atomic.Int32).Add(1) < 3 {
			return testRemote(503, "retry"), nil
		}
		return testRemote(200, "ok"), nil
	})}})
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(id int) {
			defer group.Done()
			response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: fmt.Sprintf("https://example.com/%d", id)})
			if err != nil || response == nil || !response.IsSuccess() {
				t.Errorf("response=%+v error=%v", response, err)
			}
		}(i)
	}
	group.Wait()
}
