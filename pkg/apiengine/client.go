// Package apiengine provides a reusable HTTP client for Kubernetes operators
// and other applications. It does not depend on Kubernetes APIs.
package apiengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultTimeout                = 30 * time.Second
	DefaultMaxResponseBytes int64 = 10 << 20
)

var (
	ErrUnsupportedMethod = errors.New("unsupported HTTP method")
	ErrInvalidEndpoint   = errors.New("invalid endpoint")
	ErrResponseTooLarge  = errors.New("response body exceeds configured limit")
)

// Config configures a Client. Zero values select safe defaults.
type Config struct {
	// HTTPClient allows custom transports, TLS, proxies and redirect policies.
	// Its configuration is copied; its transport is shared, not modified.
	HTTPClient *http.Client
	// Timeout overrides HTTPClient.Timeout when positive. If both are zero,
	// DefaultTimeout is used. Context deadlines can impose a shorter timeout.
	// This timeout applies per attempt; use a context deadline for a total budget.
	Timeout time.Duration
	// Headers are default request headers, copied by NewClient.
	Headers http.Header
	// MaxResponseBytes limits bytes read from a response (10 MiB by default).
	MaxResponseBytes int64
	// Retry controls application retries. The zero value disables them.
	Retry RetryConfig
}

// Request describes one API call. Endpoint must be an absolute HTTP(S) URL.
// Body is sent unchanged, making it suitable for a CRD's RawExtension.Raw
// or JSON.RawMessage. A nil or empty body sends no payload.
type Request struct {
	Method   string
	Endpoint string
	Headers  http.Header
	// Query is appended to any parameters already present in Endpoint.
	Query url.Values
	Body  []byte
}

// Response preserves the remote API result, including unsuccessful statuses.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// IsSuccess reports whether the response has a 2xx status.
func (r *Response) IsSuccess() bool {
	return r != nil && r.StatusCode >= 200 && r.StatusCode < 300
}

// DecodeJSON decodes the response body into destination.
func (r *Response) DecodeJSON(destination any) error {
	if r == nil {
		return errors.New("decode JSON: nil response")
	}
	if err := json.Unmarshal(r.Body, destination); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}
	return nil
}

// Client can be reused concurrently. Do not mutate request inputs while Do runs.
type Client struct {
	httpClient       *http.Client
	headers          http.Header
	maxResponseBytes int64
	retry            RetryConfig
}

// NewClient creates a client. Unless HTTPClient is supplied, redirects are not
// followed: the operator receives the 3xx response and decides what to do.
func NewClient(config Config) (*Client, error) {
	if config.Timeout < 0 {
		return nil, errors.New("timeout must not be negative")
	}
	if config.MaxResponseBytes < 0 || config.MaxResponseBytes == math.MaxInt64 {
		return nil, errors.New("max response bytes must be between 0 and MaxInt64-1")
	}
	retry, err := normalizeRetry(config.Retry)
	if err != nil {
		return nil, err
	}
	httpClient := http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if config.HTTPClient != nil {
		httpClient = *config.HTTPClient
	}
	if config.Timeout > 0 {
		httpClient.Timeout = config.Timeout
	} else if httpClient.Timeout == 0 {
		httpClient.Timeout = DefaultTimeout
	}
	if httpClient.Timeout < 0 {
		return nil, errors.New("HTTP client timeout must not be negative")
	}
	maxBytes := config.MaxResponseBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	return &Client{
		httpClient:       &httpClient,
		headers:          config.Headers.Clone(),
		maxResponseBytes: maxBytes,
		retry:            retry,
	}, nil
}

// Do executes GET, PUT, POST or DELETE with the supplied context.
// HTTP status codes (including 4xx/5xx) are not Go errors. Errors indicate
// invalid input, transport failures or an incomplete response body. In the
// latter case, the non-nil response contains headers and the partial body.
// Retries follow Config.Retry; exhausted HTTP statuses remain normal responses.
// Cancellation during retry backoff returns the last response and a context error.
func (c *Client) Do(ctx context.Context, input Request) (*Response, error) {
	if c == nil || c.httpClient == nil {
		return nil, errors.New("client must be initialized with NewClient")
	}
	if ctx == nil {
		return nil, errors.New("context must not be nil")
	}
	switch input.Method {
	case http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete:
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedMethod, input.Method)
	}
	endpoint, err := url.Parse(input.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	if (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return nil, fmt.Errorf("%w: use an absolute HTTP(S) URL without credentials or fragment", ErrInvalidEndpoint)
	}
	if len(input.Query) > 0 {
		query, err := url.ParseQuery(endpoint.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed query: %w", ErrInvalidEndpoint, err)
		}
		for key, values := range input.Query {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		endpoint.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, input.Method, endpoint.String(), bytes.NewReader(input.Body))
	if err != nil {
		return nil, fmt.Errorf("create API request: %w", err)
	}
	req.Header = make(http.Header)
	for key, values := range c.headers {
		req.Header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	for key, values := range input.Headers {
		req.Header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
	}
	// Content-Type is deliberately not inferred: the payload need not be JSON.
	return c.executeWithRetry(req)
}

// doOnce consumes and closes the body before a subsequent attempt can start.
func (c *Client) doOnce(req *http.Request) (*Response, error) {
	remote, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute API request: %w", err)
	}
	defer remote.Body.Close()
	response := &Response{
		StatusCode: remote.StatusCode,
		Headers:    remote.Header.Clone(),
	}
	response.Body, err = io.ReadAll(io.LimitReader(remote.Body, c.maxResponseBytes+1))
	if int64(len(response.Body)) > c.maxResponseBytes {
		response.Body = response.Body[:c.maxResponseBytes]
		return response, ErrResponseTooLarge
	}
	if err != nil {
		return response, fmt.Errorf("read API response: %w", err)
	}
	return response, nil
}
