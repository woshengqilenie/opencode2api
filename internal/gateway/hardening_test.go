package gateway

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
)

// newHardenedGateway wires a gateway against a fake upstream. Keys k1/k2 share
// the fake; the fake decides per request which one answers.
func newHardenedGateway(t *testing.T, upstreamURL string, maxAttempts int, hardening config.HardeningConfig) *Gateway {
	t.Helper()
	cfg := config.Config{
		Listen:     "127.0.0.1:0",
		ServerKeys: []string{"sk-test"},
		ZenKeys:    []string{"k1", "k2"},
		Proxies:    []string{"direct"},
		Upstream:   config.UpstreamConfig{Zen: upstreamURL, Go: upstreamURL, Anonymous: upstreamURL},
		Retry:      config.RetryConfig{MaxAttempts: maxAttempts, TimeoutSeconds: 30},
		Models:     config.ModelsConfig{RefreshSeconds: 3600, Protocols: map[string]string{}},
		Performance: config.PerformanceConfig{
			MaxIdleConns: 16, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 0,
			IdleConnTimeoutSeconds: 60, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1,
		},
		Hardening: hardening,
		Logging:   config.LoggingConfig{Level: "error", RingSize: 100},
		Prefer:    config.TierZen,
	}
	cfg.Rotation.Defaults()
	gateway, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	return gateway
}

func chatChunk(content string) string {
	return "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + content + "\"}}]}\n\n"
}

const chatDoneFrame = "data: [DONE]\n\n"

func streamRequest(t *testing.T, gateway *Gateway, body string) (*http.Response, string) {
	t.Helper()
	front := httptest.NewServer(gateway.Handler())
	defer front.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer sk-test")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	payload, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, string(payload)
}

const streamBody = `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestEmptyStreamRotatesToAnotherKey covers the headline hardening behavior: a
// first attempt that ends without content is invisible to the client, and the
// retry lands on the next key.
func TestEmptyStreamRotatesToAnotherKey(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			// First attempt: a valid SSE stream with zero output.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, chatDoneFrame)
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, chatChunk("hello")+chatDoneFrame)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 3, config.HardeningConfig{HoldFirstEventSeconds: 5, IdleEventSeconds: 10})
	response, body := streamRequest(t, gateway, streamBody)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%q", response.StatusCode, body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("client did not receive the retried content: %q", body)
	}
	if strings.Contains(body, "\"error\"") {
		t.Fatalf("the empty attempt leaked to the client: %q", body)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
}

func TestUpstreamErrorFrameRotates(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n")
			w.(http.Flusher).Flush()
			return
		}
		fmt.Fprint(w, chatChunk("recovered")+chatDoneFrame)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 3, config.HardeningConfig{HoldFirstEventSeconds: 5, IdleEventSeconds: 10})
	_, body := streamRequest(t, gateway, streamBody)
	if !strings.Contains(body, "recovered") {
		t.Fatalf("client did not receive the retried content: %q", body)
	}
	if strings.Contains(body, "overloaded") {
		t.Fatalf("the error frame leaked to the client: %q", body)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
}

// TestHoldIdleRotatesBeforeContent: the first attempt accepts the request but
// sends nothing at all; the idle watchdog aborts the hold and the request is
// retried without the client seeing the stall.
func TestHoldIdleRotatesBeforeContent(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if requests.Add(1) == 1 {
			time.Sleep(4 * time.Second)
			fmt.Fprint(w, chatChunk("late")+chatDoneFrame)
			return
		}
		fmt.Fprint(w, chatChunk("hello")+chatDoneFrame)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 3, config.HardeningConfig{HoldFirstEventSeconds: 30, IdleEventSeconds: 2})
	_, body := streamRequest(t, gateway, streamBody)
	if !strings.Contains(body, "hello") {
		t.Fatalf("client did not receive the retried content: %q", body)
	}
	if strings.Contains(body, "late") {
		t.Fatalf("the stalled attempt leaked to the client: %q", body)
	}
}

// TestFinalAttemptIsNotHeld: with a single allowed attempt there is no
// follow-up to absorb a retry, so the hold must not run — a slow first token
// must stream through instead of failing the request.
func TestFinalAttemptIsNotHeld(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(3 * time.Second)
		fmt.Fprint(w, chatChunk("slow-but-alive")+chatDoneFrame)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 1, config.HardeningConfig{HoldFirstEventSeconds: 1, IdleEventSeconds: 0})
	response, body := streamRequest(t, gateway, streamBody)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "slow-but-alive") {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
}

// TestKeepAliveFramesDuringThinkingPause: a committed stream that pauses longer
// than the keep-alive interval gets comment frames, and those frames do not
// abort the relay.
func TestKeepAliveFramesDuringThinkingPause(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, chatChunk("thinking"))
		flusher.Flush()
		time.Sleep(2500 * time.Millisecond)
		fmt.Fprint(w, chatChunk(" done")+chatDoneFrame)
		flusher.Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 3, config.HardeningConfig{HoldFirstEventSeconds: 5, IdleEventSeconds: 10, KeepAliveSeconds: 1})
	_, body := streamRequest(t, gateway, streamBody)
	if !strings.Contains(body, ": keep-alive") {
		t.Fatalf("no keep-alive frames were written: %q", body)
	}
	if !strings.Contains(body, " done") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("the stream did not finish normally: %q", body)
	}
}

// TestIdleWatchdogEndsCommittedStream: once content is committed a stall can no
// longer be retried; the watchdog ends the turn with a terminal error instead
// of letting the client hang.
func TestIdleWatchdogEndsCommittedStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, chatChunk("first"))
		flusher.Flush()
		time.Sleep(5 * time.Second)
		fmt.Fprint(w, chatChunk("should-not-arrive")+chatDoneFrame)
		flusher.Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 3, config.HardeningConfig{HoldFirstEventSeconds: 5, IdleEventSeconds: 2})
	started := time.Now()
	_, body := streamRequest(t, gateway, streamBody)
	elapsed := time.Since(started)
	// The upstream would have produced the second chunk after five seconds;
	// the request must have ended well before that.
	if elapsed > 4*time.Second {
		t.Fatalf("the stream was not aborted by the watchdog (took %s)", elapsed)
	}
	if !strings.Contains(body, "first") {
		t.Fatalf("committed content missing: %q", body)
	}
	if strings.Contains(body, "should-not-arrive") {
		t.Fatalf("the watchdog did not stop the stream: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("a terminated chat stream must still carry [DONE]: %q", body)
	}
}

// TestNonStreamZeroOutputReplays: anonymous-lane clients that ask for plain
// JSON get a collapsed document; an empty one is retried before anything is
// written downstream.
func TestNonStreamZeroOutputReplays(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if requests.Add(1) == 1 {
			// An SSE stream that carries no output at all.
			fmt.Fprint(w, chatDoneFrame)
			w.(http.Flusher).Flush()
			return
		}
		fmt.Fprint(w, chatChunk("second try")+chatDoneFrame)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 1, config.HardeningConfig{HoldFirstEventSeconds: 5, IdleEventSeconds: 10, ZeroOutputRetries: 1})
	// Force the shaped lane so the body is a collapsed SSE stream: a free
	// model name makes shapeKeyBody rewrite the request. The single attempt
	// means the hold cannot mask the replay path being exercised.
	_, body := streamRequest(t, gateway, `{"model":"test-model-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, "second try") {
		t.Fatalf("zero-output replay did not deliver content: %q", body)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
}

func TestNonStreamZeroOutputHonoursRetryBudget(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, chatDoneFrame)
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	gateway := newHardenedGateway(t, upstream.URL, 1, config.HardeningConfig{HoldFirstEventSeconds: 5, IdleEventSeconds: 10, ZeroOutputRetries: 0})
	response, body := streamRequest(t, gateway, `{"model":"test-model-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if !bytes.Contains([]byte(body), []byte("\"choices\"")) {
		t.Fatalf("expected a collapsed empty completion, got %q", body)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1 (replay disabled)", got)
	}
}
