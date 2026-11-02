package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/require"
)

func newDispatchTestLoader(t *testing.T) *forwardLoader {
	t.Helper()
	clearForwardEnv(t)
	t.Setenv("HOME", t.TempDir())
	return newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
}

func dispatchTestConfig(destinations ...forwardDestination) forwardConfig {
	return forwardConfig{forwarding: true, destinations: destinations}
}

func dispatchTestDestination(name, endpoint string) forwardDestination {
	return forwardDestination{name: name, config: forwardConfig{
		genURL: endpoint, genHeaders: map[string]string{"Authorization": "Bearer " + name},
	}}
}

func awaitDispatch(t *testing.T, events <-chan struct{}) {
	t.Helper()
	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func waitDispatch(t *testing.T, loader *forwardLoader) {
	t.Helper()
	done := make(chan struct{})
	go func() { loader.wait(); close(done) }()
	awaitDispatch(t, done)
}

func dispatchAck(w http.ResponseWriter) {
	w.Header().Set("Content-Type", wire.ContentTypeJSON)
	_, _ = io.WriteString(w, `{"results":[{"generation_id":"generation","accepted":true}]}`)
}

func TestDispatchDestinationIsolation(t *testing.T) {
	loader := newDispatchTestLoader(t)
	slow, healthy := make(chan struct{}, 16), make(chan struct{}, 16)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/slow" {
			slow <- struct{}{}
			<-release
		} else {
			healthy <- struct{}{}
		}
		dispatchAck(w)
	}))
	defer server.Close()
	defer close(release)
	loader.client = server.Client()
	cfg := dispatchTestConfig(dispatchTestDestination("slow", server.URL+"/slow"), dispatchTestDestination("healthy", server.URL+"/healthy"))
	raw := []json.RawMessage{json.RawMessage(`{"id":"generation"}`)}
	for range maxInFlightForwards {
		loader.dispatchGenerations(cfg, raw)
		awaitDispatch(t, slow)
		awaitDispatch(t, healthy)
		// The healthy response may not yet have reached its runtime.
		require.Eventually(t, func() bool {
			loader.dispatchMu.Lock()
			defer loader.dispatchMu.Unlock()
			return loader.runtimes["healthy"].requests == 0
		}, 5*time.Second, time.Millisecond)
	}
	loader.dispatchGenerations(cfg, raw)
	awaitDispatch(t, healthy)
	require.Eventually(t, func() bool {
		statuses := loader.destinationStatuses(cfg)
		return statuses[1].Legs[forwardLabelGenerations].Succeeded == maxInFlightForwards+1
	}, 5*time.Second, time.Millisecond)
	statuses := loader.destinationStatuses(cfg)
	require.EqualValues(t, 8, statuses[0].Legs[forwardLabelGenerations].Attempted)
	require.EqualValues(t, 1, statuses[0].Legs[forwardLabelGenerations].Dropped)
	require.Zero(t, statuses[0].Legs[forwardLabelGenerations].Failed)
	require.EqualValues(t, 9, statuses[1].Legs[forwardLabelGenerations].Succeeded)
	require.Empty(t, statuses[1].Failures)
	remaining := dispatchTestConfig(cfg.destinations[1])
	loader.syncDestinations(remaining)
	waitDispatch(t, loader)
	current := loader.destinationStatuses(remaining)
	require.Equal(t, statuses[1].Revision, current[0].Revision)
	require.Equal(t, statuses[1].Legs, current[0].Legs)
}

func TestDispatchPreparationSaturation(t *testing.T) {
	loader := newDispatchTestLoader(t)
	cfg := dispatchTestConfig(dispatchTestDestination("one", "https://unused.invalid"), dispatchTestDestination("two", "https://unused.invalid"))
	for i := range cfg.destinations {
		cfg.destinations[i].config.otlpEndpoint = "https://unused.invalid"
	}
	loader.syncDestinations(cfg)
	for range maxForwardPreparations {
		loader.prep <- struct{}{}
	}
	defer func() {
		for range maxForwardPreparations {
			<-loader.prep
		}
	}()
	loader.dispatchGenerations(cfg, []json.RawMessage{json.RawMessage(`{}`)})
	loader.dispatchOTLP(cfg, "metrics", wire.ContentTypeProto, "", []byte{1}, "otlp/metrics")
	waitDispatch(t, loader)
	for _, status := range loader.destinationStatuses(cfg) {
		leg := status.Legs[forwardLabelGenerations]
		require.EqualValues(t, 1, leg.Dropped)
		require.Zero(t, leg.Attempted)
		require.Zero(t, leg.Failed)
		require.EqualValues(t, 1, status.Legs["otlp/metrics"].Dropped)
		require.Zero(t, status.Legs["otlp/metrics"].Attempted)
		require.Equal(t, "preparation capacity", status.Failures[0].Detail)
	}
	loader.dispatchMu.Lock()
	requests, bytes := loader.dispatchUsageLocked()
	loader.dispatchMu.Unlock()
	require.Zero(t, requests)
	require.Zero(t, bytes)
}

func TestDispatchByteAccounting(t *testing.T) {
	loader := newDispatchTestLoader(t)
	cfg := dispatchTestConfig(dispatchTestDestination("one", "https://unused.invalid"))
	loader.syncDestinations(cfg)
	loader.dispatchMu.Lock()
	runtime := loader.runtimes["one"]
	reservation := loader.reserveDispatchLocked(runtime, "generations", 64<<20)
	require.NotNil(t, reservation)
	require.True(t, loader.chargePreparedLocked(reservation, 64<<20))
	require.Equal(t, maxForwardDestinationBytes, runtime.bytes)
	require.Nil(t, loader.reserveDispatchLocked(runtime, "generations", 1))
	require.False(t, loader.chargePreparedLocked(reservation, 1))
	lRequests, lBytes := loader.dispatchUsageLocked()
	require.Equal(t, 1, lRequests)
	require.Equal(t, maxForwardDestinationBytes, lBytes)
	loader.releaseDispatchLocked(reservation)
	require.Nil(t, loader.reserveDispatchLocked(runtime, "generations", maxForwardDestinationBytes+1))
	require.Zero(t, runtime.requests)
	require.Zero(t, runtime.bytes)
	loader.dispatchMu.Unlock()
}

func TestDispatchDaemonBoundsIncludeRetired(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(fmt.Sprint("bytes_", byBytes), func(t *testing.T) {
			loader := newDispatchTestLoader(t)
			var reservations []*forwardReservation
			for revision := range 4 {
				cfg := dispatchTestConfig(dispatchTestDestination("one", fmt.Sprintf("https://unused.invalid/%d", revision)))
				loader.syncDestinations(cfg)
				loader.dispatchMu.Lock()
				runtime := loader.runtimes["one"]
				for range maxInFlightForwards {
					var bytes int64
					if byBytes {
						bytes = maxForwardDestinationBytes / maxInFlightForwards
					}
					reservation := loader.reserveDispatchLocked(runtime, "generations", bytes)
					require.NotNil(t, reservation)
					reservations = append(reservations, reservation)
				}
				loader.dispatchMu.Unlock()
			}
			cfg := dispatchTestConfig(dispatchTestDestination("one", "https://unused.invalid/newest"))
			loader.syncDestinations(cfg)
			loader.dispatchMu.Lock()
			require.Len(t, loader.retired, 4)
			for _, retired := range loader.retired {
				require.ErrorIs(t, retired.ctx.Err(), context.Canceled)
			}
			require.Nil(t, loader.reserveDispatchLocked(loader.runtimes["one"], "generations", 1))
			requests, bytes := loader.dispatchUsageLocked()
			require.Equal(t, maxForwardDaemonRequests, requests)
			if byBytes {
				require.Equal(t, maxForwardDaemonBytes, bytes)
			}
			for _, reservation := range reservations {
				loader.releaseDispatchLocked(reservation)
			}
			require.Empty(t, loader.retired)
			admitted := loader.reserveDispatchLocked(loader.runtimes["one"], "generations", 1)
			require.NotNil(t, admitted)
			loader.releaseDispatchLocked(admitted)
			loader.dispatchMu.Unlock()
		})
	}
}

func TestDispatchDaemonPreparedByteLimit(t *testing.T) {
	loader := newDispatchTestLoader(t)
	var reservations []*forwardReservation
	// Five revisions with few requests isolate the daemon byte bound from
	// its request bound, including bytes held by retired revisions.
	for revision := range 4 {
		cfg := dispatchTestConfig(dispatchTestDestination("one", fmt.Sprintf("https://unused.invalid/%d", revision)))
		loader.syncDestinations(cfg)
		loader.dispatchMu.Lock()
		reservation := loader.reserveDispatchLocked(loader.runtimes["one"], "generations", 64<<20)
		require.NotNil(t, reservation)
		require.True(t, loader.chargePreparedLocked(reservation, 64<<20))
		reservations = append(reservations, reservation)
		loader.dispatchMu.Unlock()
	}
	cfg := dispatchTestConfig(dispatchTestDestination("one", "https://unused.invalid/newest"))
	loader.syncDestinations(cfg)
	loader.dispatchMu.Lock()
	runtime := loader.runtimes["one"]
	require.Nil(t, loader.reserveDispatchLocked(runtime, "generations", 1))
	zeroInput := loader.reserveDispatchLocked(runtime, "generations", 0)
	require.NotNil(t, zeroInput)
	require.False(t, loader.chargePreparedLocked(zeroInput, 1))
	loader.releaseDispatchLocked(zeroInput)
	loader.releaseDispatchLocked(reservations[0])
	reservation := loader.reserveDispatchLocked(runtime, "generations", 1)
	require.NotNil(t, reservation)
	require.True(t, loader.chargePreparedLocked(reservation, maxForwardDestinationBytes-1))
	require.False(t, loader.chargePreparedLocked(reservation, 1))
	loader.releaseDispatchLocked(reservation)
	for _, old := range reservations[1:] {
		loader.releaseDispatchLocked(old)
	}
	loader.dispatchMu.Unlock()
}

type dispatchRoundTripper func(*http.Request) (*http.Response, error)

func (fn dispatchRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type dispatchBodyReadSignal struct {
	io.ReadCloser
	started chan<- struct{}
	once    sync.Once
}

func (b *dispatchBodyReadSignal) Read(p []byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{} })
	return b.ReadCloser.Read(p)
}

func TestDispatchReplacementLateCompletion(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure_", fail), func(t *testing.T) {
			loader := newDispatchTestLoader(t)
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Header.Get("Authorization") == "Bearer old" {
					close(started)
					<-release
					if fail {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, "secret-token-and-prompt")
						return
					}
				}
				dispatchAck(w)
			}))
			defer server.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			loader.client = server.Client()
			transport := loader.client.Transport
			// Model a transport that finishes even after cancellation. Its late
			// result must stay on the retired runtime, success or failure.
			loader.client.Transport = dispatchRoundTripper(func(r *http.Request) (*http.Response, error) {
				return transport.RoundTrip(r.Clone(context.Background()))
			})
			old := dispatchTestDestination("one", server.URL)
			old.config.genHeaders["Authorization"] = "Bearer old"
			cfg := dispatchTestConfig(old)
			loader.dispatchGenerations(cfg, []json.RawMessage{json.RawMessage(`{"id":"generation"}`)})
			awaitDispatch(t, started)
			loader.dispatchMu.Lock()
			retired := loader.runtimes["one"]
			loader.dispatchMu.Unlock()
			newDestination := dispatchTestDestination("one", server.URL)
			cfg = dispatchTestConfig(newDestination)
			loader.syncDestinations(cfg)
			require.ErrorIs(t, retired.ctx.Err(), context.Canceled)
			loader.dispatchGenerations(cfg, []json.RawMessage{json.RawMessage(`{"id":"generation"}`)})
			require.Eventually(t, func() bool {
				return loader.destinationStatuses(cfg)[0].Legs["generations"].Succeeded == 1
			}, 5*time.Second, time.Millisecond)
			before := loader.destinationStatuses(cfg)[0]
			close(release)
			waitDispatch(t, loader)
			after := loader.destinationStatuses(cfg)[0]
			require.Equal(t, before, after)
			require.NotEqual(t, retired.revision, after.Revision)
			loader.dispatchMu.Lock()
			require.EqualValues(t, 1, retired.legs["generations"].Attempted)
			if fail {
				require.EqualValues(t, 1, retired.legs["generations"].Failed)
				require.Equal(t, "telemetry HTTP status 401", retired.failures[0].Detail)
			} else {
				require.EqualValues(t, 1, retired.legs["generations"].Succeeded)
			}
			require.Empty(t, loader.retired)
			loader.dispatchMu.Unlock()
		})
	}
}

func TestDispatchRevisionAndDetachedStatus(t *testing.T) {
	loader := newDispatchTestLoader(t)
	one := dispatchTestDestination("one", "https://unused.invalid/one")
	two := dispatchTestDestination("two", "https://unused.invalid/two")
	cfg := dispatchTestConfig(one, two)
	initial := loader.destinationStatuses(cfg)
	loader.dispatchMu.Lock()
	loader.dropDispatchLocked(loader.runtimes["one"], "generations", "capacity")
	loader.dispatchMu.Unlock()
	snapshot := loader.destinationStatuses(cfg)
	snapshot[0].Legs["generations"] = forwardLeg{}
	snapshot[0].Failures[0].Detail = "mutated"
	current := loader.destinationStatuses(dispatchTestConfig(two, one))
	require.Equal(t, initial[0].Revision, current[1].Revision)
	require.EqualValues(t, 1, current[1].Legs["generations"].Dropped)
	require.Equal(t, "capacity", current[1].Failures[0].Detail)
	changed := dispatchTestDestination("one", one.config.genURL)
	changed.config.genHeaders = map[string]string{"Authorization": "rotated"}
	current = loader.destinationStatuses(dispatchTestConfig(changed, two))
	require.NotEqual(t, initial[0].Revision, current[0].Revision)
	require.Equal(t, initial[1].Revision, current[1].Revision)
	require.Empty(t, current[0].Legs)
	policy := dispatchTestConfig(changed, two)
	policy.strip = true
	policy.forwarding = false
	require.Equal(t, current, loader.destinationStatuses(policy), "shared policy must not erase delivery history")
}

func TestDispatchStaleSnapshot(t *testing.T) {
	for _, change := range []string{"off", "enabled", "stricter", "relaxed", "unchanged", "replaced", "credentials", "removed", "invalidated", "added", "restored", "readded", "primary_replaced"} {
		for _, signal := range []string{"generations", "traces", "metrics"} {
			t.Run(change+"/"+signal, func(t *testing.T) {
				loader := newDispatchTestLoader(t)
				destination := dispatchTestDestination("one", "https://old.invalid")
				destination.config.otlpEndpoint = "https://old.invalid/otlp"
				destination.config.otlpHeaders = map[string]string{"Authorization": "Bearer old"}
				primary := dispatchTestDestination("primary", "https://primary.invalid")
				primary.config.otlpEndpoint = "https://primary.invalid/otlp"
				old := dispatchTestConfig(primary, destination)
				old.snapshot, old.strip = 1, change == "relaxed"
				old.forwarding = change != "enabled"
				initial := loader.destinationStatuses(old)
				current := old
				current.snapshot = 2
				switch change {
				case "off":
					current.forwarding = false
				case "enabled":
					current.forwarding = true
				case "stricter":
					current.strip = true
				case "relaxed":
					current.strip = false
				case "replaced", "credentials":
					replacement := destination
					if change == "replaced" {
						replacement.config.genURL = "https://new.invalid"
						replacement.config.otlpEndpoint = "https://new.invalid/otlp"
					} else {
						replacement.config.genHeaders = map[string]string{"Authorization": "Bearer new"}
						replacement.config.otlpHeaders = map[string]string{"Authorization": "Bearer new"}
					}
					current.destinations = []forwardDestination{primary, replacement}
				case "removed", "invalidated":
					current.destinations = []forwardDestination{primary}
					if change == "invalidated" {
						current.forwardingError = "forwarding.toml is unreadable"
					}
				case "added":
					added := dispatchTestDestination("new", "https://new.invalid")
					added.config.otlpEndpoint = "https://new.invalid/otlp"
					current.destinations = []forwardDestination{primary, destination, added}
				case "restored", "readded":
					intermediate := dispatchTestConfig(primary, dispatchTestDestination("one", "https://intermediate.invalid"))
					intermediate.snapshot = 2
					if change == "readded" {
						intermediate.destinations = []forwardDestination{primary}
					}
					loader.syncDestinations(intermediate)
					current.snapshot = 3
				case "primary_replaced":
					replacement := dispatchTestDestination("primary", "https://replacement.invalid")
					replacement.config.otlpEndpoint = "https://replacement.invalid/otlp"
					current.destinations = []forwardDestination{replacement, destination}
				}
				var wantNames []string
				switch change {
				case "off", "enabled", "stricter":
				case "relaxed", "unchanged", "added":
					wantNames = []string{"primary", "one"}
				case "primary_replaced":
					wantNames = []string{"one"}
				default:
					wantNames = []string{"primary"}
				}
				before := loader.destinationStatuses(current)
				targets := loader.dispatchTargets(old, signal, signal, 1)
				require.Len(t, targets, len(wantNames), "only unchanged runtimes may admit an old batch under current policy")
				var wantURLs []string
				for i, target := range targets {
					runtime := target.reservation.runtime
					require.Equal(t, wantNames[i], runtime.name)
					for _, status := range initial {
						if status.Name == runtime.name {
							require.Equal(t, status.Revision, runtime.revision)
						}
					}
					wantURLs = append(wantURLs, target.url)
					loader.dispatchMu.Lock()
					loader.releaseDispatchLocked(target.reservation)
					loader.dispatchMu.Unlock()
				}
				require.Equal(t, before, loader.destinationStatuses(old))

				calls := make(chan string, 4)
				loader.client = &http.Client{Transport: dispatchRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls <- r.URL.String()
					response := httptest.NewRecorder()
					if signal == "generations" {
						writeAcceptedGenerationResponse(response, r)
					} else {
						response.Header().Set("Content-Type", wire.ContentTypeJSON)
						_, _ = io.WriteString(response, "{}")
					}
					return response.Result(), nil
				})}
				if signal == "generations" {
					loader.dispatchGenerations(old, []json.RawMessage{json.RawMessage(`{"id":"generation"}`)})
				} else {
					loader.dispatchOTLP(old, signal, wire.ContentTypeJSON, "", []byte("{}"), signal)
				}
				waitDispatch(t, loader)
				close(calls)
				var sentURLs []string
				for endpoint := range calls {
					sentURLs = append(sentURLs, endpoint)
				}
				require.ElementsMatch(t, wantURLs, sentURLs)
				for _, status := range loader.destinationStatuses(old) {
					leg := status.Legs[signal]
					var wantCount uint64
					for _, name := range wantNames {
						if status.Name == name {
							wantCount = 1
						}
					}
					require.Equal(t, wantCount, leg.Attempted)
					require.Equal(t, wantCount, leg.Succeeded)
					require.Zero(t, leg.Failed)
					require.Zero(t, leg.Dropped)
				}
				loader.dispatchMu.Lock()
				requests, bytes := loader.dispatchUsageLocked()
				loader.dispatchMu.Unlock()
				require.Zero(t, requests)
				require.Zero(t, bytes)
			})
		}
	}
}

func TestDispatchPolicyChange(t *testing.T) {
	for _, policy := range []struct {
		name                     string
		initialStrip, strip, off bool
		restore, canceled        bool
	}{
		{name: "full_to_metadata", strip: true, canceled: true},
		{name: "full_to_metadata_to_full", strip: true, restore: true, canceled: true},
		{name: "full_to_off", off: true, canceled: true},
		{name: "full_to_off_to_full", off: true, restore: true, canceled: true},
		{name: "metadata_to_off", initialStrip: true, strip: true, off: true, canceled: true},
		{name: "metadata_to_full", initialStrip: true},
		{name: "unchanged_full"},
		{name: "unchanged_metadata", initialStrip: true, strip: true},
	} {
		for _, stage := range []string{"preparation", "delivery", "transport", "response_headers", "response_body"} {
			for _, signal := range []string{"generations", "traces", "metrics"} {
				t.Run(policy.name+"/"+stage+"/"+signal, func(t *testing.T) {
					loader := newDispatchTestLoader(t)
					activeHTTP := stage == "response_headers" || stage == "response_body"
					started, release := make(chan struct{}, 2), make(chan struct{})
					releaseHTTP := sync.OnceFunc(func() { close(release) })
					received := make(chan []byte, 2)
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						received <- body
						w.Header().Set("Content-Type", wire.ContentTypeJSON)
						if activeHTTP {
							if stage == "response_body" {
								w.(http.Flusher).Flush()
							} else {
								started <- struct{}{}
							}
							// Returning on cancellation can complete an empty OTLP response before the client's body read fails.
							<-release
						}
						if signal == "generations" {
							_, _ = io.WriteString(w, `{"results":[{"generation_id":"gen-1","accepted":true}]}`)
						} else {
							_, _ = io.WriteString(w, "{}")
						}
					}))
					defer server.Close()
					defer releaseHTTP()
					loader.client = server.Client()
					old := fakeForwardConfig(t, server.URL, policy.initialStrip)
					loader.syncDestinations(old)
					loader.dispatchMu.Lock()
					runtime := loader.runtimes["primary"]
					admissionContext := runtime.ctx
					loader.dropDispatchLocked(runtime, signal, "capacity")
					loader.dispatchMu.Unlock()
					raw := []json.RawMessage{generationRawJSON(t, contentRichGeneration(t))}
					body := []byte("{}")
					if signal == "traces" {
						body = marshalTraceRequest(t, traceRequestWithContent(), wire.ContentTypeJSON)
					}
					dispatch := func(cfg forwardConfig) {
						if signal == "generations" {
							loader.dispatchGenerations(cfg, raw)
						} else {
							loader.dispatchOTLP(cfg, signal, wire.ContentTypeJSON, "", body, signal)
						}
					}
					var targets []forwardDispatchTarget
					if stage == "transport" || stage == "response_body" {
						transport := loader.client.Transport
						loader.client.Transport = dispatchRoundTripper(func(r *http.Request) (*http.Response, error) {
							if stage == "transport" {
								started <- struct{}{}
								<-release
							}
							response, err := transport.RoundTrip(r)
							if err == nil && stage == "response_body" {
								response.Body = &dispatchBodyReadSignal{ReadCloser: response.Body, started: started}
							}
							return response, err
						})
					}
					if stage == "transport" || activeHTTP {
						dispatch(old)
						awaitDispatch(t, started)
					} else {
						targets = loader.dispatchTargets(old, signal, signal, int64(len(body)))
						require.Len(t, targets, 1)
					}
					before := loader.destinationStatuses(old)
					current := old
					current.snapshot, current.strip, current.forwarding = 2, policy.strip, !policy.off
					loader.syncDestinations(current)
					if policy.restore {
						current = old
						current.snapshot = 3
						loader.syncDestinations(current)
					}
					after := loader.destinationStatuses(current)
					if activeHTTP {
						require.Equal(t, before[0].Revision, after[0].Revision)
						require.Equal(t, before[0].Legs[signal].Dropped, after[0].Legs[signal].Dropped)
					} else {
						require.Equal(t, before, after, "policy changes must preserve delivery history")
					}
					if activeHTTP {
						if !policy.canceled {
							releaseHTTP()
						}
					} else if stage == "transport" {
						releaseHTTP()
					} else {
						payload := body
						var err error
						if signal == "generations" {
							payload, err = buildGenerationPayload(raw, old.strip)
						} else if signal == "traces" && old.strip {
							payload, _, err = stripTracePayload(body, wire.ContentTypeJSON)
						}
						require.NoError(t, err)
						if stage == "preparation" {
							loader.finishDispatchPreparation(targets, payload, wire.ContentTypeJSON, "", signal, nil)
						} else {
							loader.wg.Add(1)
							loader.deliverDispatch(targets[0], payload, wire.ContentTypeJSON, "", signal)
						}
					}
					waitDispatch(t, loader)
					releaseHTTP()
					if policy.canceled {
						require.ErrorIs(t, admissionContext.Err(), context.Canceled)
					} else {
						require.NoError(t, admissionContext.Err())
					}
					if policy.canceled && !activeHTTP {
						require.Empty(t, received, "a narrowed policy must stop admitted requests before the POST")
					} else {
						require.Len(t, received, 1)
						<-received
					}
					leg := loader.destinationStatuses(current)[0].Legs[signal]
					if policy.canceled && stage != "transport" && !activeHTTP {
						require.EqualValues(t, 2, leg.Dropped)
						require.Zero(t, leg.Attempted)
						require.Zero(t, leg.Failed)
					} else {
						require.EqualValues(t, 1, leg.Dropped)
						require.EqualValues(t, 1, leg.Attempted)
						if policy.canceled {
							require.EqualValues(t, 1, leg.Failed)
						} else {
							require.EqualValues(t, 1, leg.Succeeded)
						}
					}
					if current.forwarding {
						dispatch(current)
						waitDispatch(t, loader)
						require.Len(t, received, 1, "new batches must use an uncanceled context")
						payload := <-received
						if current.strip {
							require.NotContains(t, string(payload), "secret")
						} else if signal != "metrics" {
							require.Contains(t, string(payload), "secret")
						}
					}
					loader.dispatchMu.Lock()
					requests, bytes := loader.dispatchUsageLocked()
					loader.dispatchMu.Unlock()
					require.Zero(t, requests)
					require.Zero(t, bytes)
				})
			}
		}
	}
}

func TestDispatchOffAndPreparationFailure(t *testing.T) {
	loader := newDispatchTestLoader(t)
	cfg := dispatchTestConfig(dispatchTestDestination("one", "https://unused.invalid"))
	cfg.forwarding = false
	// enabled deliberately differs: only the global forwarding switch controls
	// dispatch, and destinations remain visible while forwarding is off.
	cfg.enabled = true
	loader.dispatchGenerations(cfg, []json.RawMessage{json.RawMessage(`{}`)})
	require.Len(t, loader.destinationStatuses(cfg), 1)
	require.Empty(t, loader.destinationStatuses(cfg)[0].Legs)
	cfg.forwarding, cfg.strip = true, true
	for range maxRecordedForwardFailures + 2 {
		loader.dispatchGenerations(cfg, []json.RawMessage{json.RawMessage(`{"metadata":"secret-token-and-prompt"}`)})
		waitDispatch(t, loader)
	}
	status := loader.destinationStatuses(cfg)[0]
	require.EqualValues(t, maxRecordedForwardFailures+2, status.Legs["generations"].Dropped)
	require.Zero(t, status.Legs["generations"].Failed)
	require.Zero(t, status.Legs["generations"].Attempted)
	require.Len(t, status.Failures, maxRecordedForwardFailures)
	encoded, err := json.Marshal(status)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret-token-and-prompt")
	loader.dispatchMu.Lock()
	requests, bytes := loader.dispatchUsageLocked()
	loader.dispatchMu.Unlock()
	require.Zero(t, requests)
	require.Zero(t, bytes)
}

func TestDispatchOTLPAndLegCounters(t *testing.T) {
	loader := newDispatchTestLoader(t)
	type received struct{ path, contentType, encoding, marker, auth, body string }
	requests := make(chan received, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- received{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), r.Header.Get(ForwardMarkerHeader), r.Header.Get("Authorization"), string(body)}
		if strings.Contains(r.URL.Path, "reject") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "secret-token-and-prompt")
			return
		}
		w.Header().Set("Content-Type", wire.ContentTypeJSON)
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	loader.client = server.Client()
	cfg := dispatchTestConfig(forwardDestination{name: "one", config: forwardConfig{
		otlpEndpoint: server.URL + "/otlp", otlpHeaders: map[string]string{"Authorization": "Bearer otlp"}, genURL: server.URL + "/reject",
	}})
	cfg.strip = true
	loader.dispatchOTLP(cfg, "traces", wire.ContentTypeJSON, "gzip", gzipBytes(t, []byte(`{"resourceSpans":[]}`)), forwardLabelOTLPGenerations)
	waitDispatch(t, loader)
	trace := <-requests
	require.Equal(t, "/otlp/v1/traces", trace.path)
	require.Equal(t, wire.ContentTypeJSON, trace.contentType)
	require.Empty(t, trace.encoding)
	require.JSONEq(t, `{}`, trace.body)
	require.Equal(t, "1", trace.marker)
	require.Equal(t, "Bearer otlp", trace.auth)
	metrics := gzipBytes(t, []byte(`{"resourceMetrics":[]}`))
	loader.dispatchOTLP(cfg, "metrics", wire.ContentTypeJSON, "gzip", metrics, "otlp/metrics")
	waitDispatch(t, loader)
	metric := <-requests
	require.Equal(t, "/otlp/v1/metrics", metric.path)
	require.Equal(t, "gzip", metric.encoding)
	require.Equal(t, string(metrics), metric.body)
	loader.dispatchGenerations(cfg, []json.RawMessage{json.RawMessage(`{}`)})
	waitDispatch(t, loader)
	<-requests
	status := loader.destinationStatuses(cfg)[0]
	require.EqualValues(t, 1, status.Legs[forwardLabelOTLPGenerations].Succeeded)
	require.EqualValues(t, 1, status.Legs["otlp/metrics"].Succeeded)
	require.EqualValues(t, 1, status.Legs["generations"].Attempted)
	require.EqualValues(t, 1, status.Legs["generations"].Failed)
	require.Zero(t, status.Legs["generations"].Dropped)
	require.Len(t, status.Failures, 1)
	require.Equal(t, "one", status.Failures[0].Destination)
	require.Equal(t, "telemetry HTTP status 400", status.Failures[0].Detail)
}

func TestDispatchPreparedCapacityDropReleasesReservation(t *testing.T) {
	loader := newDispatchTestLoader(t)
	cfg := dispatchTestConfig(dispatchTestDestination("one", "https://unused.invalid"))
	targets := loader.dispatchTargets(cfg, "generations", "generations", maxForwardDestinationBytes-1)
	require.Len(t, targets, 1)
	loader.finishDispatchPreparation(targets, []byte("{}"), wire.ContentTypeJSON, "", "generations", nil)
	status := loader.destinationStatuses(cfg)[0]
	require.EqualValues(t, 1, status.Legs["generations"].Dropped)
	require.Zero(t, status.Legs["generations"].Attempted)
	require.Zero(t, status.Legs["generations"].Failed)
	loader.dispatchMu.Lock()
	requests, bytes := loader.dispatchUsageLocked()
	loader.dispatchMu.Unlock()
	require.Zero(t, requests)
	require.Zero(t, bytes)
}

func TestDispatchFailureCategoriesAreSanitized(t *testing.T) {
	for _, test := range []struct{ detail, category string }{
		{"telemetry HTTP status 401", "telemetry HTTP status 401"},
		{"OTLP partial success: rejected_spans=12", "OTLP partial success: rejected_spans=12"},
		{"generation acknowledgement identity mismatch", "generation acknowledgement identity mismatch"},
		{"read OTLP response: secret-token-and-prompt", "delivery failed"},
		{"telemetry HTTP status 401 secret-token-and-prompt", "delivery failed"},
		{"OTLP partial success: rejected_spans=12 secret-token-and-prompt", "delivery failed"},
	} {
		require.Equal(t, test.category, forwardDispatchErrorCategory(errors.New(test.detail)))
	}
	require.Equal(t, "canceled", forwardDispatchErrorCategory(fmt.Errorf("secret-token-and-prompt: %w", context.Canceled)))
}
