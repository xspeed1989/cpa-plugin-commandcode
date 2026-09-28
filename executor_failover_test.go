package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type failoverHTTPClient struct {
	keys               []string
	failCalls          int
	status             int
	body               []byte
	transportErr       error
	streamErr          error
	cancel             context.CancelFunc
	payloadBeforeError bool
}

func (c *failoverHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	c.keys = append(c.keys, req.Headers.Get("Authorization"))
	if c.cancel != nil {
		c.cancel()
	}
	if len(c.keys) <= c.failCalls {
		body := c.body
		if body == nil {
			body = []byte(`{"error":{"message":"upstream failed"}}`)
		}
		return pluginapi.HTTPResponse{StatusCode: c.status, Body: body}, c.transportErr
	}
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"choices":[]}`)}, nil
}

func (c *failoverHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	resp, err := c.Do(ctx, req)
	chunks := make(chan pluginapi.HTTPStreamChunk, 2)
	body := resp.Body
	if resp.StatusCode == http.StatusOK {
		body = append(append([]byte("data: "), body...), '\n')
	}
	if len(c.keys) <= c.failCalls && c.streamErr != nil {
		if c.payloadBeforeError {
			chunks <- pluginapi.HTTPStreamChunk{Payload: body}
		}
		chunks <- pluginapi.HTTPStreamChunk{Err: c.streamErr}
	} else {
		chunks <- pluginapi.HTTPStreamChunk{Payload: body}
	}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Chunks: chunks}, err
}

func runFailoverRequest(ctx context.Context, c *failoverHTTPClient, stream bool) error {
	e := NewExecutor(parseConfig([]byte("api_keys:\n  - key: fake-a\n  - key: fake-b\n")), nil)
	req := pluginapi.ExecutorRequest{Model: "deepseek-flash", Payload: []byte(`{"messages":[]}`), HTTPClient: c}
	if !stream {
		_, err := e.Execute(ctx, req)
		return err
	}
	resp, err := e.ExecuteStream(ctx, req)
	if err != nil {
		return err
	}
	for chunk := range resp.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return nil
}

func TestExecutorFailoverClassification(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		retries bool
	}{
		{name: "unauthorized", status: 401, body: `{}`, retries: true},
		{name: "payment required", status: 402, body: `{"error":{"message":"Payment required"}}`, retries: true},
		{name: "rate limited", status: 429, body: `{}`, retries: true},
		{name: "upstream failure", status: 500, body: `{}`, retries: true},
		{name: "upstream unavailable", status: 503, body: `{}`, retries: true},
		{name: "forbidden quota code", status: 403, body: `{"error":{"code":"insufficient_quota","message":"Account exhausted"}}`, retries: true},
		{name: "forbidden credits", status: 403, body: `{"error":{"message":"Insufficient credits. Please top up."}}`, retries: true},
		{name: "forbidden balance", status: 403, body: `{"error":{"message":"Your credit balance is too low"}}`, retries: true},
		{name: "forbidden quota text", status: 403, body: `quota exceeded`, retries: true},
		{name: "bad request quota body", status: 400, body: `{"error":{"code":"insufficient_quota"}}`, retries: true},
		{name: "forbidden permission", status: 403, body: `{"error":{"code":"permission_denied","message":"Model access denied"}}`, retries: false},
		{name: "bad request", status: 400, body: `{"error":{"message":"messages is required"}}`, retries: false},
		{name: "not found", status: 404, body: `{"error":{"message":"model not found"}}`, retries: false},
		{name: "unprocessable", status: 422, body: `{"error":{"message":"invalid tool schema"}}`, retries: false},
		{name: "redirect", status: 300, body: ``, retries: false},
		{name: "forbidden empty body", status: 403, body: ``, retries: false},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				c := &failoverHTTPClient{status: tt.status, body: []byte(tt.body), failCalls: 1}
				err := runFailoverRequest(t.Context(), c, stream)
				wantCalls := 1
				if tt.retries {
					wantCalls = 2
				}
				if len(c.keys) != wantCalls {
					t.Fatalf("upstream calls=%d, want %d; err=%v", len(c.keys), wantCalls, err)
				}
				if tt.retries {
					if err != nil {
						t.Fatal(err)
					}
					if c.keys[0] == c.keys[1] {
						t.Fatal("retried the same key instead of switching accounts")
					}
					return
				}
				se, ok := err.(statusError)
				if !ok || se.statusCode != tt.status || string(se.body) != tt.body {
					t.Fatalf("original upstream error not preserved: %v", err)
				}
			})
		}
	}
}

func TestExecutorRetryRounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, failCalls := range []int{2, 4, 6} {
			t.Run(fmt.Sprintf("failures=%d/stream=%v", failCalls, stream), func(t *testing.T) {
				t.Parallel()
				c := &failoverHTTPClient{status: 402, failCalls: failCalls}
				start := time.Now()
				err := runFailoverRequest(t.Context(), c, stream)
				wantCalls := failCalls + 1
				if failCalls == 6 {
					wantCalls = 6
					se, ok := err.(statusError)
					if !ok || se.statusCode != 402 || string(se.body) != `{"error":{"message":"upstream failed"}}` {
						t.Fatalf("last error not preserved: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if len(c.keys) != wantCalls {
					t.Fatalf("calls=%d, want %d", len(c.keys), wantCalls)
				}
				for i := 0; i+1 < len(c.keys); i += 2 {
					if c.keys[i] == c.keys[i+1] {
						t.Fatalf("same account repeated within round: %v", c.keys)
					}
				}
				minDelay := time.Second
				if failCalls >= 4 {
					minDelay = 3 * time.Second
				}
				if time.Since(start) < minDelay {
					t.Fatalf("round backoff missing: %v", time.Since(start))
				}
			})
		}
	}
}

func TestExecutorTransportErrorFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		c := &failoverHTTPClient{failCalls: 1, transportErr: errors.New("connection reset")}
		if err := runFailoverRequest(t.Context(), c, stream); err != nil {
			t.Fatal(err)
		}
		if len(c.keys) != 2 {
			t.Fatalf("calls=%d, want 2", len(c.keys))
		}
	}
}

func TestExecutorCancellationStopsRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := &failoverHTTPClient{status: 402, failCalls: 6, cancel: cancel}
			if err := runFailoverRequest(ctx, c, stream); !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, want cancellation", err)
			}
			if len(c.keys) != 1 {
				t.Fatalf("calls=%d after cancellation", len(c.keys))
			}

			ctx2, cancel2 := context.WithCancel(t.Context())
			cancel2()
			c2 := &failoverHTTPClient{}
			if err := runFailoverRequest(ctx2, c2, stream); !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, want cancellation", err)
			}
			if len(c2.keys) != 0 {
				t.Fatal("sent a request with canceled context")
			}
		})
	}
}

func TestExecutorCancellationDuringBackoff(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			c := &failoverHTTPClient{status: 402, failCalls: 6}
			start := time.Now()
			if err := runFailoverRequest(ctx, c, stream); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			if len(c.keys) != 2 {
				t.Fatalf("calls=%d, want 2", len(c.keys))
			}
			if time.Since(start) >= time.Second {
				t.Fatal("backoff did not stop on cancellation")
			}
		})
	}
}

func TestExecutorStreamErrorBoundary(t *testing.T) {
	sentinel := errors.New("stream disconnected")
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("outputStarted=%v", started), func(t *testing.T) {
			c := &failoverHTTPClient{status: 200, failCalls: 1, streamErr: sentinel, payloadBeforeError: started}
			err := runFailoverRequest(t.Context(), c, true)
			if started {
				if !errors.Is(err, sentinel) || len(c.keys) != 1 {
					t.Fatalf("mid-stream failure must not retry: calls=%d err=%v", len(c.keys), err)
				}
			} else if err != nil || len(c.keys) != 2 {
				t.Fatalf("failure before output must retry: calls=%d err=%v", len(c.keys), err)
			}
		})
	}
}
