package apiengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultInitialBackoff = 100 * time.Millisecond
	DefaultMaxBackoff     = 5 * time.Second
)

// RetryConfig configures bounded application-level retries.
type RetryConfig struct {
	// MaxAttempts includes the initial call. Zero or one disables retries.
	MaxAttempts int
	// InitialBackoff defaults to 100ms; MaxBackoff defaults to 5s.
	// Exponential backoff uses equal jitter (50-100% of the capped delay).
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// RetryPOSTWithIdempotencyKey enables POST retries only when a non-empty
	// Idempotency-Key header is present. Enable only if the remote API enforces
	// idempotency for that key. GET, PUT and DELETE are eligible by default.
	RetryPOSTWithIdempotencyKey bool
}

func normalizeRetry(config RetryConfig) (RetryConfig, error) {
	if config.MaxAttempts < 0 || config.InitialBackoff < 0 || config.MaxBackoff < 0 {
		return RetryConfig{}, errors.New("retry attempts and backoff must not be negative")
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 1
	}
	if config.InitialBackoff == 0 {
		config.InitialBackoff = DefaultInitialBackoff
	}
	if config.MaxBackoff == 0 {
		config.MaxBackoff = DefaultMaxBackoff
	}
	if config.MaxBackoff < config.InitialBackoff {
		return RetryConfig{}, errors.New("retry max backoff must be at least initial backoff")
	}
	return config, nil
}

func (c *Client) executeWithRetry(req *http.Request) (*Response, error) {
	ctx := req.Context()
	eligible := req.Method != http.MethodPost || (c.retry.RetryPOSTWithIdempotencyKey && strings.TrimSpace(req.Header.Get("Idempotency-Key")) != "")
	backoff := c.retry.InitialBackoff
	var lastResponse *Response
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return lastResponse, fmt.Errorf("execute API request: %w", err)
		}
		// Clone headers and reconstruct the body so every attempt sends the
		// identical payload and idempotency key, even after a transport failure.
		attemptRequest := req.Clone(ctx)
		if req.GetBody != nil {
			var err error
			attemptRequest.Body, err = req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("replay API request body: %w", err)
			}
		}
		response, err := c.doOnce(attemptRequest)
		lastResponse = response
		if attempt >= c.retry.MaxAttempts || !eligible || !shouldRetry(response, err) {
			return response, err
		}
		delay := jitter(backoff)
		if response != nil {
			if retryAfter := retryAfterDelay(response.Headers.Get("Retry-After"), time.Now()); retryAfter > delay {
				delay = retryAfter
			}
		}
		if err := waitForRetry(ctx, delay); err != nil {
			return response, fmt.Errorf("wait for API retry: %w", err)
		}
		backoff = nextBackoff(backoff, c.retry.MaxBackoff)
	}
}

func shouldRetry(response *Response, err error) bool {
	if err != nil {
		// Never retry partial/oversized responses, invalid input, caller
		// cancellation, certificate failures or arbitrary application errors.
		if response != nil || errors.Is(err, context.Canceled) {
			return false
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return true
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return true
		}
		for _, transient := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
			if errors.Is(err, transient) {
				return true
			}
		}
		return false
	}
	if response == nil {
		return false
	}
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	// Compare before doubling to avoid duration overflow.
	if current >= maximum-current {
		return maximum
	}
	return current * 2
}

func jitter(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

// retryAfterDelay accepts delta-seconds or an HTTP date. It is not capped by
// MaxBackoff: never retry earlier than the server requested. The caller's
// context deadline bounds the overall wait, including very large values.
func retryAfterDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	// Only unsigned decimal integers are valid delta-seconds.
	digits := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
