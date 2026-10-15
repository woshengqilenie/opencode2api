package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wire "opencode2api/internal/protocol"
)

// Hold / watchdog sentinel errors. They are recorded as attempt failures and
// classified for the request log.
var (
	errHoldNoContent  = errors.New("upstream stream ended before producing any content")
	errHoldSSEError   = errors.New("upstream stream carried an error before producing content")
	errHoldTimeout    = errors.New("upstream produced no content before the hold timeout")
	errHoldIdle       = errors.New("upstream stream went idle before producing content")
	errRelayIdle      = errors.New("upstream stream went idle after content was committed")
	errHoldUnarmGuard = errors.New("hardening guards are disabled")
)

// holdEnabled reports whether the pre-content hold should run at all. Both
// guards disabled means the operator asked for the historical pass-through
// behavior: an attempt is handed to the relay the moment its headers arrive.
func (g *Gateway) holdEnabled() bool {
	return g.cfg.Hardening.HoldFirstEventSeconds > 0 || g.cfg.Hardening.IdleEventSeconds > 0
}

// holdOutcome reports what the pre-content hold observed.
type holdOutcome struct {
	prefix []byte
	class  wire.StreamHeadClass
	err    error
}

// prefixedBody replays bytes the hold already consumed before the live body.
// Close delegates to the underlying body so the attempt cancellation and
// connection reuse semantics of attemptBody are preserved.
type prefixedBody struct {
	io.Reader
	closer io.Closer
}

func (body *prefixedBody) Close() error {
	return body.closer.Close()
}

// holdBody consumes the head of an upstream SSE response until it produces
// content, ends, or a guard fires. It never writes to the client, so a
// non-content outcome can be retried on another node without the client
// noticing. The idle watchdog and the absolute cap both abort by closing the
// body, which is safe precisely because a guard firing means the attempt is
// about to be discarded.
func (g *Gateway) holdBody(body io.ReadCloser, from wire.Protocol) holdOutcome {
	guards := g.cfg.Hardening
	var (
		stateMu   sync.Mutex
		released  bool
		aborted   bool
		wasIdle   atomic.Bool
		stopGuard = make(chan struct{})
	)
	defer close(stopGuard)

	abort := func(idle bool) {
		stateMu.Lock()
		if released || aborted {
			stateMu.Unlock()
			return
		}
		aborted = true
		if idle {
			wasIdle.Store(true)
		}
		stateMu.Unlock()
		_ = body.Close()
	}

	var capTimer *time.Timer
	if guards.HoldFirstEventSeconds > 0 {
		capTimer = time.AfterFunc(time.Duration(guards.HoldFirstEventSeconds)*time.Second, func() { abort(false) })
	}
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	if guards.IdleEventSeconds > 0 {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopGuard:
					return
				case <-ticker.C:
					if time.Since(time.Unix(0, lastRead.Load())) < time.Duration(guards.IdleEventSeconds)*time.Second {
						continue
					}
					abort(true)
					return
				}
			}
		}()
	}

	prefix, class, err := wire.HoldFirstContent(body, from, func() { lastRead.Store(time.Now().UnixNano()) })
	if capTimer != nil {
		capTimer.Stop()
	}
	stateMu.Lock()
	released = true
	didAbort, idle := aborted, wasIdle.Load()
	stateMu.Unlock()

	outcome := holdOutcome{prefix: prefix, class: class, err: err}
	switch {
	case err == nil && class == wire.StreamHeadContent:
		return outcome
	case didAbort && idle:
		outcome.err = errHoldIdle
	case didAbort:
		outcome.err = errHoldTimeout
	case err != nil:
		// keep the stream error
	case class == wire.StreamHeadError:
		outcome.err = errHoldSSEError
	default:
		outcome.err = errHoldNoContent
	}
	return outcome
}

// idleWatch aborts a committed stream when upstream bytes stop arriving. The
// downstream headers are already out, so the abort surfaces as the relay's
// terminal error handling; the flag lets telemetry name the real cause.
type idleWatch struct {
	fired atomic.Bool
	stop  chan struct{}
}

// startIdleWatch wraps the body so every read refreshes the activity clock and
// closes the body when the clock goes stale. Closing an in-flight HTTP body is
// the only portable way to interrupt a blocked Read.
func startIdleWatch(body io.ReadCloser, idle time.Duration) (*idleWatch, io.Reader) {
	watch := &idleWatch{stop: make(chan struct{})}
	if idle <= 0 {
		return watch, body
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watch.stop:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, last.Load())) < idle {
					continue
				}
				watch.fired.Store(true)
				_ = body.Close()
				return
			}
		}
	}()
	return watch, progressBody{reader: body, onRead: func() { last.Store(time.Now().UnixNano()) }}
}

func (watch *idleWatch) Fired() bool {
	return watch != nil && watch.fired.Load()
}

func (watch *idleWatch) Stop() {
	if watch == nil {
		return
	}
	select {
	case <-watch.stop:
	default:
		close(watch.stop)
	}
}

// progressBody reports successful reads to the idle clock.
type progressBody struct {
	reader io.Reader
	onRead func()
}

func (p progressBody) Read(buffer []byte) (int, error) {
	n, err := p.reader.Read(buffer)
	if n > 0 && p.onRead != nil {
		p.onRead()
	}
	return n, err
}

// keepAliveWriter serializes downstream writes and interleaves `: keep-alive`
// comment frames while no real bytes have gone out for a full interval. SSE
// comments are ignored by every compliant client, so the frames are
// invisible except to their read timeouts.
//
// net/http response writers are not goroutine-safe, so every write and every
// flush — from the relay and from the keep-alive loop alike — goes through the
// same mutex, and Stop waits for the loop to exit before the handler returns.
type keepAliveWriter struct {
	writer   http.ResponseWriter
	flusher  http.Flusher
	interval time.Duration

	mu      sync.Mutex
	last    time.Time
	stopped bool
	stop    chan struct{}
	done    sync.WaitGroup
}

func newKeepAliveWriter(writer http.ResponseWriter, interval time.Duration) *keepAliveWriter {
	k := &keepAliveWriter{writer: writer, interval: interval, last: time.Now(), stop: make(chan struct{})}
	if flusher, ok := writer.(http.Flusher); ok {
		k.flusher = flusher
	}
	if interval > 0 && k.flusher != nil {
		k.done.Add(1)
		go k.loop()
	}
	return k
}

func (k *keepAliveWriter) Header() http.Header {
	return k.writer.Header()
}

func (k *keepAliveWriter) WriteHeader(status int) {
	k.writer.WriteHeader(status)
}

func (k *keepAliveWriter) Write(data []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.last = time.Now()
	return k.writer.Write(data)
}

// Flush satisfies http.Flusher for the relay helpers, which flush after every
// frame. It must take the same lock as Write: flushing concurrently with the
// keep-alive loop races inside net/http.
func (k *keepAliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.flusher != nil {
		k.flusher.Flush()
	}
}

func (k *keepAliveWriter) loop() {
	defer k.done.Done()
	ticker := time.NewTicker(min(k.interval, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
		}
		// Re-check under the lock: Stop may have run while this tick was
		// pending, and nothing may touch the response writer after the
		// handler returns.
		k.mu.Lock()
		if k.stopped {
			k.mu.Unlock()
			return
		}
		if time.Since(k.last) >= k.interval {
			if _, err := io.WriteString(k.writer, ": keep-alive\n\n"); err == nil {
				k.flusher.Flush()
				k.last = time.Now()
			}
		}
		k.mu.Unlock()
	}
}

func (k *keepAliveWriter) Stop() {
	k.mu.Lock()
	if !k.stopped {
		k.stopped = true
		close(k.stop)
	}
	k.mu.Unlock()
	k.done.Wait()
}

// holdIfStream runs the pre-content hold on a 2xx SSE response when the
// guards are armed and a later attempt could absorb a retry. It reports
// held=true when the attempt produced no content and must fail over; the
// caller drains the body and cools the node. On success the consumed prefix
// is wrapped back in front of the live body, so the relay sees the untouched
// byte stream.
func (g *Gateway) holdIfStream(resp *http.Response, from wire.Protocol, followUpExists bool) (holdOutcome, bool) {
	if resp == nil {
		return holdOutcome{}, false
	}
	if !g.holdEnabled() || !followUpExists {
		return holdOutcome{}, false
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return holdOutcome{}, false
	}
	outcome := g.holdBody(resp.Body, from)
	if outcome.err == nil && outcome.class == wire.StreamHeadContent {
		resp.Body = bufferedBody(outcome.prefix, resp.Body)
		return outcome, false
	}
	return outcome, true
}

// bufferedBody turns a hold prefix plus a live body into one closeable reader.
func bufferedBody(prefix []byte, body io.ReadCloser) io.ReadCloser {
	if len(prefix) == 0 {
		return body
	}
	return &prefixedBody{Reader: io.MultiReader(bytes.NewReader(prefix), body), closer: body}
}
