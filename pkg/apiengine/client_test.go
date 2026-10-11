package apiengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T, config Config) *Client {
	t.Helper()
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete} {
		for _, body := range []string{"", `{"name":"example"}`} {
			t.Run(method+"/"+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					actual, err := io.ReadAll(r.Body)
					if err != nil || string(actual) != body || r.Method != method {
						t.Errorf("received %s %q, error %v", r.Method, actual, err)
					}
					if r.URL.Query().Get("existing") != "yes" || r.URL.Query().Get("q") != "a b&c" || len(r.URL.Query()["tag"]) != 2 {
						t.Errorf("unexpected query: %v", r.URL.Query())
					}
					if r.Header.Get("Authorization") != "Bearer override" || r.Header.Get("X-Default") != "default" || r.Header.Get("Content-Type") != "application/json" {
						t.Errorf("unexpected headers: %v", r.Header)
					}
					w.Header().Set("X-Result", "ok")
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":123}`)
				}))
				defer server.Close()
				client := newTestClient(t, Config{Headers: http.Header{
					"Authorization": {"Bearer default"}, "X-Default": {"default"},
				}})
				response, err := client.Do(context.Background(), Request{
					Method: method, Endpoint: server.URL + "?existing=yes&tag=first",
					Query:   url.Values{"q": {"a b&c"}, "tag": {"second"}},
					Headers: http.Header{"authorization": {"Bearer override"}, "Content-Type": {"application/json"}},
					Body:    []byte(body),
				})
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusCreated || !response.IsSuccess() || response.Headers.Get("X-Result") != "ok" {
					t.Fatalf("unexpected response: %+v", response)
				}
				var result struct{ ID int }
				if err := response.DecodeJSON(&result); err != nil || result.ID != 123 {
					t.Fatalf("decode: %+v, %v", result, err)
				}
			})
		}
	}
}

func TestHTTPStatusesAreNotErrors(t *testing.T) {
	for _, status := range []int{204, 301, 400, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			response, err := newTestClient(t, Config{}).Do(context.Background(), Request{Method: http.MethodGet, Endpoint: server.URL})
			if err != nil || response.StatusCode != status || response.IsSuccess() != (status >= 200 && status < 300) {
				t.Fatalf("response=%+v error=%v", response, err)
			}
		})
	}
}

func TestInvalidRequests(t *testing.T) {
	client := newTestClient(t, Config{})
	for _, endpoint := range []string{"", "/relative", "ftp://example.com", "http://", "http://user:password@example.com", "https://example.com/#fragment", "http://[::1"} {
		_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: endpoint})
		if !errors.Is(err, ErrInvalidEndpoint) {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
	}
	for _, method := range []string{"", "get", http.MethodPatch, http.MethodHead} {
		_, err := client.Do(context.Background(), Request{Method: method, Endpoint: "https://example.com"})
		if !errors.Is(err, ErrUnsupportedMethod) {
			t.Errorf("method %q: %v", method, err)
		}
	}
	// A zero-value context deliberately verifies invalid input handling.
	var nilContext context.Context
	if _, err := client.Do(nilContext, Request{}); err == nil {
		t.Fatal("expected nil context error")
	}
	if _, err := (&Client{}).Do(context.Background(), Request{}); err == nil {
		t.Fatal("expected uninitialized client error")
	}
	_, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com?bad=%zz", Query: url.Values{"q": {"value"}}})
	if !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("malformed query: %v", err)
	}
}

func TestConfig(t *testing.T) {
	for _, config := range []Config{{Timeout: -1}, {MaxResponseBytes: -1}, {MaxResponseBytes: math.MaxInt64}, {HTTPClient: &http.Client{Timeout: -1}}} {
		if _, err := NewClient(config); err == nil {
			t.Errorf("expected invalid config error: %+v", config)
		}
	}
	client := newTestClient(t, Config{})
	if client.httpClient.Timeout != DefaultTimeout || client.maxResponseBytes != DefaultMaxResponseBytes {
		t.Fatal("unexpected defaults")
	}
	original := &http.Client{Timeout: time.Minute}
	headers := http.Header{"X-Default": {"original"}}
	client = newTestClient(t, Config{HTTPClient: original, Timeout: time.Second, Headers: headers})
	headers.Set("X-Default", "changed")
	if original.Timeout != time.Minute || client.httpClient.Timeout != time.Second || client.headers.Get("X-Default") != "original" {
		t.Fatal("configuration was not copied")
	}
	if newTestClient(t, Config{HTTPClient: original}).httpClient.Timeout != time.Minute {
		t.Fatal("custom client timeout not preserved")
	}
}

func TestCancellationAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newTestClient(t, Config{}).Do(ctx, Request{Method: http.MethodGet, Endpoint: server.URL})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	_, err = newTestClient(t, Config{Timeout: 50 * time.Millisecond}).Do(context.Background(), Request{Method: http.MethodGet, Endpoint: server.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestResponseLimit(t *testing.T) {
	for _, body := range []string{"1234", "12345", "123456"} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			response, err := newTestClient(t, Config{MaxResponseBytes: 5}).Do(context.Background(), Request{Method: http.MethodGet, Endpoint: server.URL})
			if len(body) > 5 {
				if !errors.Is(err, ErrResponseTooLarge) || string(response.Body) != body[:5] {
					t.Fatalf("limit: response=%+v error=%v", response, err)
				}
			} else if err != nil || string(response.Body) != body {
				t.Fatalf("response=%+v error=%v", response, err)
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer server.Close()
	for _, test := range []struct {
		config Config
		status int
	}{{Config{}, http.StatusFound}, {Config{HTTPClient: server.Client()}, http.StatusNoContent}} {
		response, err := newTestClient(t, test.config).Do(context.Background(), Request{Method: http.MethodGet, Endpoint: server.URL})
		if err != nil || response.StatusCode != test.status {
			t.Fatalf("redirect: response=%+v error=%v", response, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{ closed bool }

func (b *brokenBody) Read(p []byte) (int, error) { return copy(p, "partial"), io.ErrUnexpectedEOF }
func (b *brokenBody) Close() error               { b.closed = true; return nil }

func TestPartialResponseAndClose(t *testing.T) {
	body := &brokenBody{}
	client := newTestClient(t, Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Test": {"value"}}, Body: body}, nil
	})}})
	response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com"})
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(response.Body) != "partial" || !body.closed || response.Headers.Get("X-Test") != "value" {
		t.Fatalf("partial response=%+v error=%v closed=%v", response, err, body.closed)
	}
}

func TestTransportError(t *testing.T) {
	failure := errors.New("transport failure")
	client := newTestClient(t, Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, failure
	})}})
	response, err := client.Do(context.Background(), Request{Method: http.MethodGet, Endpoint: "https://example.com"})
	if response != nil || !errors.Is(err, failure) {
		t.Fatalf("response=%+v error=%v", response, err)
	}
}

func TestConcurrentRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	client := newTestClient(t, Config{Headers: http.Header{"X-Default": {"value"}}})
	input := Request{Method: http.MethodPost, Endpoint: server.URL, Body: []byte("payload"), Headers: http.Header{"X-Request": {"value"}}}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := client.Do(context.Background(), input)
			if err != nil || string(response.Body) != "ok" {
				t.Errorf("response=%+v error=%v", response, err)
			}
		}()
	}
	group.Wait()
}

func TestDecodeJSON(t *testing.T) {
	var result any
	for _, response := range []*Response{nil, {Body: []byte("not json")}, {Body: nil}} {
		if err := response.DecodeJSON(&result); err == nil {
			t.Fatal("expected JSON error")
		}
	}
	if (*Response)(nil).IsSuccess() {
		t.Fatal("nil response must not be successful")
	}
	if err := (&Response{Body: []byte(`{}`)}).DecodeJSON(nil); err == nil || !strings.Contains(err.Error(), "decode JSON") {
		t.Fatalf("invalid destination: %v", err)
	}
}
