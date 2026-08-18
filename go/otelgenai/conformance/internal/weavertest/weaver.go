package weavertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotInstalled reports that Weaver is not available on PATH.
var ErrNotInstalled = errors.New("weaver is not installed")

// Violation is one deduplicated violation-level Weaver finding.
type Violation struct {
	ID         string `json:"id"`
	Message    string `json:"message"`
	Context    any    `json:"context,omitempty"`
	SignalName string `json:"signal_name,omitempty"`
	SignalType string `json:"signal_type,omitempty"`
	Count      int    `json:"count"`
}

// Report is the JSON document returned by Weaver's stop endpoint.
type Report struct {
	Raw map[string]any
}

// MarshalJSON writes the original Weaver report.
func (r Report) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Raw)
}

// Violations returns deduplicated violation-level findings from every sample.
func (r Report) Violations() []Violation {
	type group struct {
		finding Violation
		key     string
	}
	groups := make(map[string]*group)
	walkReport(r.Raw, func(object map[string]any) {
		result, ok := object["live_check_result"].(map[string]any)
		if !ok {
			return
		}
		advice, ok := result["all_advice"].([]any)
		if !ok {
			return
		}
		for _, item := range advice {
			finding, ok := item.(map[string]any)
			if !ok || stringValue(finding["level"]) != "violation" {
				continue
			}
			contextJSON, _ := json.Marshal(finding["context"])
			violation := Violation{
				ID:         stringValue(finding["id"]),
				Message:    stringValue(finding["message"]),
				Context:    finding["context"],
				SignalName: stringValue(finding["signal_name"]),
				SignalType: stringValue(finding["signal_type"]),
				Count:      1,
			}
			key := violation.ID + "\x00" + violation.Message + "\x00" + string(contextJSON) + "\x00" + violation.SignalName + "\x00" + violation.SignalType
			if existing := groups[key]; existing != nil {
				existing.finding.Count++
				continue
			}
			groups[key] = &group{finding: violation, key: key}
		}
	})

	out := make([]Violation, 0, len(groups))
	for _, grouped := range groups {
		out = append(out, grouped.finding)
	}
	slices.SortFunc(out, func(a, b Violation) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		if a.Message < b.Message {
			return -1
		}
		if a.Message > b.Message {
			return 1
		}
		return 0
	})
	return out
}

// SpanOperationCounts returns gen_ai.operation.name counts from span samples.
func (r Report) SpanOperationCounts() map[string]int {
	counts := make(map[string]int)
	samples, _ := r.Raw["samples"].([]any)
	for _, sample := range samples {
		entry, _ := sample.(map[string]any)
		span, _ := entry["span"].(map[string]any)
		attrs, _ := span["attributes"].([]any)
		for _, rawAttr := range attrs {
			attr, _ := rawAttr.(map[string]any)
			if stringValue(attr["name"]) == "gen_ai.operation.name" {
				counts[stringValue(attr["value"])]++
				break
			}
		}
	}
	return counts
}

// SeenMetricNames returns registry metrics that had at least one data point.
func (r Report) SeenMetricNames() map[string]struct{} {
	out := make(map[string]struct{})
	statistics, _ := r.Raw["statistics"].(map[string]any)
	seen, _ := statistics["seen_registry_metrics"].(map[string]any)
	for name, rawCount := range seen {
		count, _ := rawCount.(float64)
		if count > 0 {
			out[name] = struct{}{}
		}
	}
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func walkReport(value any, visit func(map[string]any)) {
	switch value := value.(type) {
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkReport(child, visit)
		}
	case []any:
		for _, child := range value {
			walkReport(child, visit)
		}
	}
}

// Weaver is one running weaver registry live-check process.
type Weaver struct {
	command  *exec.Cmd
	wait     <-chan error
	endpoint string
	adminURL string
	stdout   bytes.Buffer
	stderr   bytes.Buffer

	mu      sync.Mutex
	stopped bool
}

// Start launches Weaver and waits for its health endpoint.
func Start(ctx context.Context, assets Assets) (*Weaver, error) {
	binary, err := exec.LookPath("weaver")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotInstalled, err)
	}

	const maxAttempts = 3
	var lastErr error
	for range maxAttempts {
		weaver, err := startWeaver(ctx, binary, assets)
		if err == nil {
			return weaver, nil
		}
		lastErr = err
		if !isAddressInUse(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("start Weaver after %d port-allocation attempts: %w", maxAttempts, lastErr)
}

func startWeaver(ctx context.Context, binary string, assets Assets) (*Weaver, error) {
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	otlpPort, adminPort := ports[0], ports[1]
	weaver := &Weaver{
		endpoint: net.JoinHostPort("127.0.0.1", strconv.Itoa(otlpPort)),
		adminURL: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(adminPort)),
	}
	weaver.command = exec.Command(binary,
		"registry", "live-check",
		"--inactivity-timeout=30",
		"--otlp-grpc-address=127.0.0.1",
		"--otlp-grpc-port="+strconv.Itoa(otlpPort),
		"--admin-port="+strconv.Itoa(adminPort),
		"--output=http",
		"--format=json",
		"--registry", assets.Registry,
		"--config", assets.Config,
		"--advice-policies", assets.Policies,
		"--advice-data", assets.AdviceData,
	)
	weaver.command.Stdout = &weaver.stdout
	weaver.command.Stderr = &weaver.stderr
	if err := weaver.command.Start(); err != nil {
		return nil, fmt.Errorf("start Weaver: %w", err)
	}
	wait := make(chan error, 1)
	weaver.wait = wait
	go func() { wait <- weaver.command.Wait() }()

	exited, readyErr := weaver.waitForReady(ctx)
	if readyErr == nil {
		return weaver, nil
	}
	if !exited {
		_ = weaver.command.Process.Kill()
		<-weaver.wait
	}
	return nil, fmt.Errorf("%w\nstdout:\n%s\nstderr:\n%s", readyErr, weaver.stdout.String(), weaver.stderr.String())
}

func isAddressInUse(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

// Endpoint is the host and port of Weaver's OTLP gRPC listener.
func (w *Weaver) Endpoint() string {
	return w.endpoint
}

func (w *Weaver) waitForReady(ctx context.Context) (bool, error) {
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.adminURL+"/health", nil)
		if err != nil {
			return false, err
		}
		response, requestErr := client.Do(req)
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return false, nil
			}
		}
		select {
		case err := <-w.wait:
			return true, fmt.Errorf("Weaver exited before becoming ready: %v", err)
		case <-ticker.C:
		case <-timeout.C:
			return false, errors.New("Weaver did not become ready in 10 seconds")
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// End stops Weaver and returns its full JSON report. A violation can make the
// process exit nonzero, but it does not prevent the report from being returned.
func (w *Weaver) End(ctx context.Context) (Report, error) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return Report{}, errors.New("Weaver is already stopped")
	}
	w.stopped = true
	w.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.adminURL+"/stop", nil)
	if err != nil {
		return Report{}, err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return Report{}, fmt.Errorf("stop Weaver: %w\nstderr:\n%s", err, w.stderr.String())
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Report{}, fmt.Errorf("stop Weaver: HTTP %s", response.Status)
	}
	var raw map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		return Report{}, fmt.Errorf("decode Weaver report: %w", err)
	}

	select {
	case waitErr := <-w.wait:
		var exitErr *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exitErr) {
			return Report{}, fmt.Errorf("wait for Weaver: %w", waitErr)
		}
	case <-ctx.Done():
		_ = w.command.Process.Kill()
		return Report{}, ctx.Err()
	}
	return Report{Raw: raw}, nil
}

// Close terminates Weaver when a scenario exits before End.
func (w *Weaver) Close() {
	w.mu.Lock()
	stopped := w.stopped
	w.mu.Unlock()
	if !stopped {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = w.End(ctx)
		cancel()
	}
	if w.command.ProcessState == nil && w.command.Process != nil {
		_ = w.command.Process.Kill()
	}
}

func freePorts(count int) ([]int, error) {
	listeners := make([]net.Listener, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, fmt.Errorf("find free port: %w", err)
		}
		listeners = append(listeners, listener)
	}

	ports := make([]int, 0, count)
	var closeErr error
	for _, listener := range listeners {
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
		closeErr = errors.Join(closeErr, listener.Close())
	}
	if closeErr != nil {
		return nil, fmt.Errorf("release reserved ports: %w", closeErr)
	}
	return ports, nil
}
