package apiengine_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	apiengine "github.com/karavy/api-engine/pkg/apiengine"
)

func ExampleClient_Do() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"resource-123"}`)
	}))
	defer server.Close()

	client, err := apiengine.NewClient(apiengine.Config{})
	if err != nil {
		panic(err)
	}
	response, err := client.Do(context.Background(), apiengine.Request{
		Method:   http.MethodPost,
		Endpoint: server.URL + "/resources",
		Headers:  http.Header{"Content-Type": {"application/json"}},
		Body:     []byte(`{"name":"example"}`),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(response.StatusCode, response.IsSuccess(), string(response.Body))
	// Output: 201 true {"id":"resource-123"}
}

func ExampleClient_Do_withRetries() {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"resource-123"}`)
	}))
	defer server.Close()

	client, err := apiengine.NewClient(apiengine.Config{
		Timeout: time.Second,
		Retry: apiengine.RetryConfig{
			MaxAttempts:    3,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     10 * time.Millisecond,
			// Only enable this if the real API deduplicates using the key.
			RetryPOSTWithIdempotencyKey: true,
		},
	})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.Do(ctx, apiengine.Request{
		Method:   http.MethodPost,
		Endpoint: server.URL + "/resources",
		Headers: http.Header{
			"Content-Type":    {"application/json"},
			"Idempotency-Key": {"resource-123-create-v1"},
		},
		Body: []byte(`{"name":"example"}`),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(response.StatusCode, attempts.Load())
	// Output: 201 2
}
