package local

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServer_Events_NotifiesOnGeneration boots the server behind
// httptest.NewServer (httptest.NewRecorder cannot stream), subscribes
// to the SSE endpoint, posts a generation, and asserts the broadcast
// frame carries the expected identifiers.
func TestServer_Events_NotifiesOnGeneration(t *testing.T) {
	s, _ := newTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := openEventStream(t, ctx, ts.URL)
	defer stream.close()

	// Wait for the initial ":\n\n" comment so the subscription is
	// registered on the server before we trigger a broadcast.
	require.NoError(t, stream.waitForComment(2*time.Second))

	body := `{"generations":[{"id":"gen-1","conversation_id":"conv-A"}]}`
	postResp, err := http.Post(ts.URL+"/api/v1/generations:export", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	postResp.Body.Close()

	ev, err := stream.nextEvent(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, "conv-A", ev.ConversationID)
	assert.Equal(t, "gen-1", ev.GenerationID)
}

// TestServer_Events_SetsSSEHeaders verifies the response advertises the
// SSE content type and disables proxy buffering so frames are not held
// behind reverse proxies.
func TestServer_Events_SetsSSEHeaders(t *testing.T) {
	s, _ := newTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
}

// TestServer_Events_ClosesOnClientCancel asserts the handler returns
// when the client disconnects so a dropped browser tab does not leak a
// goroutine.
func TestServer_Events_ClosesOnClientCancel(t *testing.T) {
	s, _ := newTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stream := openEventStream(t, ctx, ts.URL)
	require.NoError(t, stream.waitForComment(2*time.Second))

	cancel()

	// The next read should fail (canceled context drains the body).
	if err := stream.waitForEOF(2 * time.Second); err != nil {
		t.Fatalf("stream did not close after client cancel: %v", err)
	}
	stream.close()
}

// TestServer_Events_ClosesOnServerClose asserts Server.Close drops open
// streams so the daemon's shutdown is not blocked by an idle viewer.
func TestServer_Events_ClosesOnServerClose(t *testing.T) {
	s, _ := newTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := openEventStream(t, ctx, ts.URL)
	defer stream.close()
	require.NoError(t, stream.waitForComment(2*time.Second))

	s.Close()

	if err := stream.waitForEOF(2 * time.Second); err != nil {
		t.Fatalf("stream did not close after Server.Close: %v", err)
	}
}

// TestServer_Events_BroadcastsEveryGenerationInBurst posts an export
// with two generations and asserts both broadcast frames arrive on the
// stream, so a burst export does not collapse into a single event on
// the server side.
func TestServer_Events_BroadcastsEveryGenerationInBurst(t *testing.T) {
	s, _ := newTestServer(t)
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := openEventStream(t, ctx, ts.URL)
	defer stream.close()
	require.NoError(t, stream.waitForComment(2*time.Second))

	body := `{"generations":[
		{"id":"gen-1","conversation_id":"conv-A"},
		{"id":"gen-2","conversation_id":"conv-B"}
	]}`
	postResp, err := http.Post(ts.URL+"/api/v1/generations:export", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	postResp.Body.Close()

	first, err := stream.nextEvent(2 * time.Second)
	require.NoError(t, err)
	second, err := stream.nextEvent(2 * time.Second)
	require.NoError(t, err)

	got := []changeEvent{first, second}
	assert.Contains(t, got, changeEvent{ConversationID: "conv-A", GenerationID: "gen-1"})
	assert.Contains(t, got, changeEvent{ConversationID: "conv-B", GenerationID: "gen-2"})
}

// TestServer_Events_HeartbeatComment exercises the ping branch of the
// handler's select. Without this test the heartbeat path is dead code
// in the suite — it triggers only on idle streams.
func TestServer_Events_HeartbeatComment(t *testing.T) {
	s, _ := newTestServer(t)
	s.eventPingInterval = 50 * time.Millisecond
	ts := httptest.NewServer(s)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := openEventStream(t, ctx, ts.URL)
	defer stream.close()
	require.NoError(t, stream.waitForComment(2*time.Second))

	// A second comment frame must arrive without any broadcast.
	require.NoError(t, stream.waitForComment(2*time.Second))
}

// TestEventHub_FanOutDeliversToAllSubscribers asserts a single
// broadcast reaches every subscriber rather than landing on one
// subscriber arbitrarily — the multi-viewer story for the daemon.
func TestEventHub_FanOutDeliversToAllSubscribers(t *testing.T) {
	hub := newEventHub()
	sub1 := hub.subscribe()
	sub2 := hub.subscribe()
	require.NotNil(t, sub1)
	require.NotNil(t, sub2)

	hub.broadcast(changeEvent{ConversationID: "conv-X", GenerationID: "gen-X"})

	want := changeEvent{ConversationID: "conv-X", GenerationID: "gen-X"}
	select {
	case ev := <-sub1.ch:
		assert.Equal(t, want, ev)
	case <-time.After(time.Second):
		t.Fatal("sub1 did not receive broadcast")
	}
	select {
	case ev := <-sub2.ch:
		assert.Equal(t, want, ev)
	case <-time.After(time.Second):
		t.Fatal("sub2 did not receive broadcast")
	}
}

// TestServer_Events_HubClosedReturnsServiceUnavailable verifies that a
// subscribe attempt after shutdown is rejected with 503 instead of
// hanging the connection.
func TestServer_Events_HubClosedReturnsServiceUnavailable(t *testing.T) {
	s, _ := newTestServer(t)
	s.Close()
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestEventHub_BroadcastDropsOnFullBuffer exercises the hub directly:
// fills one subscriber's buffer and asserts broadcast still returns
// without blocking, and that the overflow event is silently dropped for
// that subscriber.
func TestEventHub_BroadcastDropsOnFullBuffer(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe()
	require.NotNil(t, sub)

	// Fill the buffer.
	for range eventSubBuffer {
		hub.broadcast(changeEvent{GenerationID: "filler"})
	}

	// Overflow broadcast must not block.
	done := make(chan struct{})
	go func() {
		hub.broadcast(changeEvent{GenerationID: "overflow"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked on full subscriber buffer")
	}

	// Drain and confirm the overflow event was the one dropped: the
	// queued frames are the original fillers.
	for range eventSubBuffer {
		ev := <-sub.ch
		assert.Equal(t, "filler", ev.GenerationID)
	}
	select {
	case ev := <-sub.ch:
		t.Fatalf("expected drained channel, got %+v", ev)
	default:
	}
}

// TestEventHub_CloseAllIsIdempotent asserts repeated closeAll calls do
// not panic on already-closed subscriber channels.
func TestEventHub_CloseAllIsIdempotent(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe()
	require.NotNil(t, sub)

	hub.closeAll()
	// Subscriber channel must be closed.
	_, ok := <-sub.ch
	assert.False(t, ok, "subscriber channel should be closed by closeAll")

	// Second closeAll must be a no-op, not a double-close panic.
	hub.closeAll()

	// Subscribe after close returns nil so callers do not hold a dead
	// subscription against the hub.
	assert.Nil(t, hub.subscribe())

	// Broadcast after close is a no-op (would panic if it tried to send
	// to a closed channel).
	hub.broadcast(changeEvent{GenerationID: "after-close"})
}

// TestEventHub_UnsubscribeAfterCloseAll asserts the late-unsubscribe
// path (handler running closeAll's cleanup via defer) does not
// double-close a channel closeAll already closed.
func TestEventHub_UnsubscribeAfterCloseAll(t *testing.T) {
	hub := newEventHub()
	sub := hub.subscribe()
	require.NotNil(t, sub)
	hub.closeAll()
	// Must not panic.
	hub.unsubscribe(sub)
}

// TestEventHub_ConcurrentBroadcast is a smoke test for the hub under
// concurrent broadcasters and subscribers — guards against a future
// regression that drops the mutex around the subscriber map.
func TestEventHub_ConcurrentBroadcast(t *testing.T) {
	hub := newEventHub()
	const subs = 4
	const events = 200

	var wg sync.WaitGroup
	for range subs {
		sub := hub.subscribe()
		require.NotNil(t, sub)
		wg.Go(func() {
			for range sub.ch {
				// Drain; we only care that no race or deadlock occurs.
			}
		})
	}

	for range events {
		hub.broadcast(changeEvent{GenerationID: "x"})
	}
	hub.closeAll()
	wg.Wait()
}

// eventStream is a tiny SSE reader used in tests. It owns the response
// and a background goroutine that pumps lines into a channel so tests
// can wait with explicit deadlines instead of hanging on Read.
type eventStream struct {
	resp  *http.Response
	lines chan readResult
	close func()
}

type readResult struct {
	line string
	err  error
}

func openEventStream(t *testing.T, ctx context.Context, baseURL string) *eventStream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/events", nil)
	require.NoError(t, err)
	// The returned eventStream owns resp.Body and closes it via close();
	// the linter can't follow the indirection.
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	lines := make(chan readResult, 32)
	reader := bufio.NewReader(resp.Body)
	go func() {
		defer close(lines)
		for {
			l, err := reader.ReadString('\n')
			if l != "" {
				lines <- readResult{line: l}
			}
			if err != nil {
				lines <- readResult{err: err}
				return
			}
		}
	}()
	return &eventStream{
		resp:  resp,
		lines: lines,
		close: func() { _ = resp.Body.Close() },
	}
}

// waitForComment reads until the initial ":\n" SSE comment + blank
// terminator arrives, signalling the handler is past subscribe(). Used
// to avoid a race where a test broadcasts before the subscriber is
// actually registered.
func (s *eventStream) waitForComment(timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		select {
		case res, ok := <-s.lines:
			if !ok {
				return io.EOF
			}
			if res.err != nil {
				return res.err
			}
			if res.line == ":\n" {
				// Consume the blank line that terminates the SSE
				// message so subsequent reads start cleanly.
				select {
				case term := <-s.lines:
					if term.err != nil {
						return term.err
					}
				case <-deadline:
					return context.DeadlineExceeded
				}
				return nil
			}
		case <-deadline:
			return context.DeadlineExceeded
		}
	}
}

// nextEvent reads lines until a "data:" frame terminates with a blank
// line, then decodes the JSON payload.
func (s *eventStream) nextEvent(timeout time.Duration) (changeEvent, error) {
	deadline := time.After(timeout)
	var payload strings.Builder
	for {
		select {
		case res, ok := <-s.lines:
			if !ok {
				return changeEvent{}, io.EOF
			}
			if res.err != nil {
				return changeEvent{}, res.err
			}
			switch {
			case strings.HasPrefix(res.line, "data:"):
				payload.WriteString(strings.TrimSpace(strings.TrimPrefix(res.line, "data:")))
			case res.line == "\n":
				if payload.Len() > 0 {
					var ev changeEvent
					if err := json.Unmarshal([]byte(payload.String()), &ev); err != nil {
						return changeEvent{}, err
					}
					return ev, nil
				}
			}
		case <-deadline:
			return changeEvent{}, context.DeadlineExceeded
		}
	}
}

// waitForEOF drains until the read goroutine reports an error (the
// connection closed). Used to assert lifecycle teardown.
func (s *eventStream) waitForEOF(timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		select {
		case res, ok := <-s.lines:
			if !ok {
				return nil
			}
			if res.err != nil {
				return nil
			}
		case <-deadline:
			return context.DeadlineExceeded
		}
	}
}
