package local

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
)

const (
	maxForwardDestinationBytes int64 = 128 << 20
	maxForwardDaemonRequests         = 32
	maxForwardDaemonBytes      int64 = 512 << 20
	maxForwardPreparations           = 2
)

type forwardDestinationStatus struct {
	ForwardDestinationInfo
	Revision uint64                `json:"revision"`
	Legs     map[string]forwardLeg `json:"legs,omitempty"`
	Failures []forwardFailure      `json:"failures,omitempty"`
}

// All runtime state, including retired runtimes, is guarded by dispatchMu.
// A reservation keeps its runtime alive until preparation and delivery finish.
type forwardRuntime struct {
	name        string
	fingerprint [sha256.Size]byte
	revision    uint64
	// firstSnapshot prevents an old batch from entering a removed and recreated route.
	firstSnapshot uint64
	ctx           context.Context
	cancel        context.CancelFunc
	requests      int
	bytes         int64
	legs          map[string]forwardLeg
	failures      []forwardFailure
}

type forwardReservation struct {
	runtime *forwardRuntime
	// Policy relaxation must not revive reservations canceled by an earlier policy.
	ctx   context.Context
	label string
	bytes int64
}

type forwardDispatchTarget struct {
	reservation *forwardReservation
	url         string
	headers     map[string]string
}

// The revision fingerprint includes only destination URLs and headers. Reordering
// destinations or editing a refusal message must not reset a healthy destination's counters.
func destinationFingerprint(cfg forwardConfig, _, _ bool) [sha256.Size]byte {
	data, _ := json.Marshal(struct {
		Generations string
		GenHeaders  map[string]string
		OTLP        string
		OTLPHeaders map[string]string
	}{cfg.genURL, cfg.genHeaders, cfg.otlpEndpoint, cfg.otlpHeaders})
	return sha256.Sum256(data)
}

func (l *forwardLoader) syncDestinations(cfg forwardConfig) {
	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	l.syncDestinationsLocked(cfg)
}

func (l *forwardLoader) syncDestinationsLocked(cfg forwardConfig) {
	if cfg.snapshot != 0 && cfg.snapshot < l.dispatchSnapshot {
		return
	}
	policyNarrowed := (l.dispatchConfig.forwarding && !cfg.forwarding) || (!l.dispatchConfig.strip && cfg.strip)
	l.dispatchSnapshot, l.dispatchConfig = cfg.snapshot, cfg
	if l.runtimes == nil {
		l.runtimes = make(map[string]*forwardRuntime)
	}
	if l.prep == nil {
		l.prep = make(chan struct{}, maxForwardPreparations)
	}
	wanted := make(map[string]bool, len(cfg.destinations))
	for _, destination := range cfg.destinations {
		wanted[destination.name] = true
		fingerprint := destinationFingerprint(destination.config, cfg.forwarding, cfg.strip)
		if runtime := l.runtimes[destination.name]; runtime != nil {
			if runtime.fingerprint == fingerprint {
				if policyNarrowed {
					runtime.cancel()
					runtime.ctx, runtime.cancel = context.WithCancel(context.Background())
				}
				continue
			}
			l.retireDestinationLocked(runtime)
		}
		l.revision++
		ctx, cancel := context.WithCancel(context.Background())
		l.runtimes[destination.name] = &forwardRuntime{
			name: destination.name, fingerprint: fingerprint, revision: l.revision, firstSnapshot: cfg.snapshot,
			ctx: ctx, cancel: cancel, legs: make(map[string]forwardLeg),
		}
	}
	for name, runtime := range l.runtimes {
		if !wanted[name] {
			l.retireDestinationLocked(runtime)
			delete(l.runtimes, name)
		}
	}
}

func (l *forwardLoader) retireDestinationLocked(runtime *forwardRuntime) {
	runtime.cancel()
	if runtime.requests != 0 {
		l.retired = append(l.retired, runtime)
	}
}

// destinationStatuses returns detached maps and slices: callers never observe
// delivery goroutines mutating a previously returned status.
func (l *forwardLoader) destinationStatuses(cfg forwardConfig) []forwardDestinationStatus {
	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	l.syncDestinationsLocked(cfg)
	cfg = l.dispatchConfig
	out := make([]forwardDestinationStatus, 0, len(cfg.destinations))
	for _, destination := range cfg.destinations {
		runtime := l.runtimes[destination.name]
		failures := slices.Clone(runtime.failures)
		slices.Reverse(failures)
		out = append(out, forwardDestinationStatus{
			ForwardDestinationInfo: ForwardDestinationInfo{
				Name: destination.name, Endpoint: forwardStatusEndpoint(destination.config.genURL),
				OTLPEndpoint:    forwardStatusEndpoint(destination.config.otlpEndpoint),
				HTTPGenerations: destination.config.genURL != "",
				OTLP:            destination.config.otlpEndpoint != "",
			},
			Revision: runtime.revision, Legs: maps.Clone(runtime.legs), Failures: failures,
		})
	}
	return out
}

func forwardStatusEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

func (l *forwardLoader) dispatchUsageLocked() (int, int64) {
	var requests int
	var bytes int64
	for _, runtime := range l.runtimes {
		requests += runtime.requests
		bytes += runtime.bytes
	}
	for _, runtime := range l.retired {
		requests += runtime.requests
		bytes += runtime.bytes
	}
	return requests, bytes
}

func (l *forwardLoader) reserveDispatchLocked(runtime *forwardRuntime, label string, bytes int64) *forwardReservation {
	requests, total := l.dispatchUsageLocked()
	if bytes < 0 || runtime.requests >= maxInFlightForwards || requests >= maxForwardDaemonRequests ||
		bytes > maxForwardDestinationBytes-runtime.bytes || bytes > maxForwardDaemonBytes-total {
		l.dropDispatchLocked(runtime, label, "capacity")
		return nil
	}
	runtime.requests++
	runtime.bytes += bytes
	return &forwardReservation{runtime: runtime, ctx: runtime.ctx, label: label, bytes: bytes}
}

func (l *forwardLoader) chargePreparedLocked(reservation *forwardReservation, bytes int64) bool {
	_, total := l.dispatchUsageLocked()
	if bytes < 0 || bytes > maxForwardDestinationBytes-reservation.runtime.bytes || bytes > maxForwardDaemonBytes-total {
		return false
	}
	reservation.bytes += bytes
	reservation.runtime.bytes += bytes
	return true
}

func (l *forwardLoader) releaseDispatchLocked(reservation *forwardReservation) {
	runtime := reservation.runtime
	runtime.requests--
	runtime.bytes -= reservation.bytes
	l.retired = slices.DeleteFunc(l.retired, func(runtime *forwardRuntime) bool { return runtime.requests == 0 })
}

func (l *forwardLoader) dispatchFailureLocked(runtime *forwardRuntime, label, category string) {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	leg := runtime.legs[label]
	leg.LastFailureAt, leg.LastFailureDetail = at, category
	runtime.legs[label] = leg
	runtime.failures = append(runtime.failures, forwardFailure{
		At: at, Label: label, Detail: category, Destination: runtime.name,
	})
	kept := 0
	for i, failure := range slices.Backward(runtime.failures) {
		if failure.Label == label {
			kept++
			if kept > maxRecordedForwardFailures {
				runtime.failures = slices.Delete(runtime.failures, i, i+1)
			}
		}
	}
}

func (l *forwardLoader) dropDispatchLocked(runtime *forwardRuntime, label, category string) {
	leg := runtime.legs[label]
	leg.Dropped++
	runtime.legs[label] = leg
	l.dispatchFailureLocked(runtime, label, category)
}

func (l *forwardLoader) dispatchTargets(cfg forwardConfig, signal, label string, inputBytes int64) []forwardDispatchTarget {
	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	l.syncDestinationsLocked(cfg)
	if !cfg.forwarding || !l.dispatchConfig.forwarding || (l.dispatchConfig.strip && !cfg.strip) {
		return nil
	}
	var targets []forwardDispatchTarget
	for _, destination := range cfg.destinations {
		endpoint, headers := destination.config.genURL, destination.config.genHeaders
		if signal != "generations" {
			endpoint, headers = destination.config.otlpEndpoint, destination.config.otlpHeaders
			if endpoint != "" {
				endpoint = otel.SignalEndpointURL(endpoint, signal)
			}
		}
		if endpoint == "" {
			continue
		}
		runtime := l.runtimes[destination.name]
		if runtime == nil || runtime.fingerprint != destinationFingerprint(destination.config, false, false) ||
			(cfg.snapshot != 0 && cfg.snapshot < runtime.firstSnapshot) {
			continue
		}
		if reservation := l.reserveDispatchLocked(runtime, label, inputBytes); reservation != nil {
			targets = append(targets, forwardDispatchTarget{reservation, endpoint, headers})
		}
	}
	return targets
}

// Receiver-owned inputs stay immutable until delivery completes. Admission
// precedes asynchronous preparation, which never waits for capacity.
func (l *forwardLoader) beginDispatchPreparation(targets []forwardDispatchTarget) bool {
	if len(targets) == 0 {
		return false
	}
	select {
	case l.prep <- struct{}{}:
		l.wg.Add(1)
		return true
	default:
		l.dispatchMu.Lock()
		defer l.dispatchMu.Unlock()
		for _, target := range targets {
			reservation := target.reservation
			l.dropDispatchLocked(reservation.runtime, reservation.label, "preparation capacity")
			l.releaseDispatchLocked(reservation)
		}
		return false
	}
}

func (l *forwardLoader) dispatchGenerations(cfg forwardConfig, raw []json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var inputBytes int64
	for _, generation := range raw {
		inputBytes += int64(len(generation))
	}
	targets := l.dispatchTargets(cfg, "generations", forwardLabelGenerations, inputBytes)
	if !l.beginDispatchPreparation(targets) {
		return
	}
	go func() {
		defer l.wg.Done()
		payload, err := buildGenerationPayload(raw, cfg.strip)
		<-l.prep
		l.finishDispatchPreparation(targets, payload, wire.ContentTypeJSON, "", "generations", err)
	}()
}

func (l *forwardLoader) dispatchOTLP(cfg forwardConfig, signal, contentType, contentEncoding string, body []byte, label string) {
	if len(body) == 0 || (signal != "traces" && signal != "metrics") {
		return
	}
	targets := l.dispatchTargets(cfg, signal, label, int64(len(body)))
	if !l.beginDispatchPreparation(targets) {
		return
	}
	go func() {
		defer l.wg.Done()
		payload, ct, encoding := body, contentType, contentEncoding
		var err error
		if signal == "traces" && cfg.strip {
			if isGzipEncoding(encoding) {
				payload, err = gunzipLimited(payload, maxOTLPTraceBodyBytes)
			}
			if err == nil {
				payload, ct, err = stripTracePayload(payload, ct)
			}
			encoding = ""
		}
		if strings.TrimSpace(ct) == "" {
			ct = wire.ContentTypeProto
		}
		<-l.prep
		l.finishDispatchPreparation(targets, payload, ct, encoding, signal, err)
	}()
}

func (l *forwardLoader) finishDispatchPreparation(targets []forwardDispatchTarget, payload []byte, contentType, encoding, signal string, err error) {
	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	for _, target := range targets {
		reservation := target.reservation
		runtime := reservation.runtime
		category := ""
		switch {
		case reservation.ctx.Err() != nil:
			category = "canceled"
		case err != nil:
			category = "preparation failed"
		case !l.chargePreparedLocked(reservation, int64(len(payload))):
			category = "capacity"
		}
		if category != "" {
			l.dropDispatchLocked(runtime, reservation.label, category)
			l.releaseDispatchLocked(reservation)
			continue
		}
		l.wg.Add(1)
		go l.deliverDispatch(target, payload, contentType, encoding, signal)
	}
}

// Response bodies and transport errors may contain credentials or captured
// content. Only fixed categories and validated numeric diagnostics reach status.
func forwardDispatchErrorCategory(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if transportError, ok := errors.AsType[*telemetryTransportError](err); ok {
		return transportError.Error()
	}
	detail := err.Error()
	switch detail {
	case "invalid telemetry request", "telemetry request canceled or timed out", "telemetry transport failed",
		"generation acknowledgement could not be read or exceeds 1 MiB", "invalid generation acknowledgement", "invalid generation payload",
		"generation acknowledgement result count mismatch", "generation acknowledgement identity mismatch",
		"generation acknowledgement rejected records", "invalid OTLP traces response", "invalid OTLP metrics response":
		return detail
	}
	for _, prefix := range []string{
		"telemetry HTTP status ", "OTLP partial success: rejected_spans=", "OTLP partial success: rejected_data_points=",
	} {
		if value, ok := strings.CutPrefix(detail, prefix); ok {
			if number, err := strconv.ParseUint(value, 10, 64); err == nil {
				return prefix + strconv.FormatUint(number, 10)
			}
		}
	}
	return "delivery failed"
}

func (l *forwardLoader) deliverDispatch(target forwardDispatchTarget, payload []byte, contentType, encoding, signal string) {
	defer l.wg.Done()
	reservation := target.reservation
	runtime := reservation.runtime
	l.dispatchMu.Lock()
	if reservation.ctx.Err() != nil {
		l.dropDispatchLocked(runtime, reservation.label, "canceled")
		l.releaseDispatchLocked(reservation)
		l.dispatchMu.Unlock()
		return
	}
	leg := runtime.legs[reservation.label]
	leg.Attempted++
	runtime.legs[reservation.label] = leg
	l.dispatchMu.Unlock()

	err := l.sendTelemetry(reservation.ctx, target.url, contentType, encoding, target.headers, payload, signal)

	l.dispatchMu.Lock()
	defer l.dispatchMu.Unlock()
	defer l.releaseDispatchLocked(reservation)
	leg = runtime.legs[reservation.label]
	if err != nil {
		leg.Failed++
		runtime.legs[reservation.label] = leg
		l.dispatchFailureLocked(runtime, reservation.label, forwardDispatchErrorCategory(err))
		return
	}
	leg.Succeeded++
	leg.LastSuccessAt = time.Now().UTC().Format(time.RFC3339Nano)
	runtime.legs[reservation.label] = leg
	runtime.failures = slices.DeleteFunc(runtime.failures, func(failure forwardFailure) bool {
		return failure.Label == reservation.label
	})
}
