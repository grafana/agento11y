package experiments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultControlPath      = "/api/plugins/grafana-sigil-app/resources/eval"
	portabilityMetadataKey  = "agento11y.sdk.portability"
	portabilityVersion      = 1
	maxControlResponseBytes = 8 << 20
)

var (
	ErrValidation = errors.New("agento11y experiments: validation failed")
	ErrNotFound   = errors.New("agento11y experiments: not found")
	ErrConflict   = errors.New("agento11y experiments: conflict")
	ErrTransport  = errors.New("agento11y experiments: transport failed")
)

type ConflictKind string

const (
	ConflictScoreCountMismatch ConflictKind = "score_count_mismatch"
	ConflictRunningTrials      ConflictKind = "running_trials"
	ConflictTerminalState      ConflictKind = "terminal_state"
	ConflictImmutableField     ConflictKind = "immutable_field"
	ConflictOpenDraft          ConflictKind = "open_draft"
	ConflictUnknown            ConflictKind = "unknown"
)

type ConflictError struct {
	Kind    ConflictKind
	Message string
}

func (e *ConflictError) Error() string { return e.Message }
func (e *ConflictError) Unwrap() error { return ErrConflict }

func ClassifyConflict(err error) ConflictKind {
	var typed *ConflictError
	if errors.As(err, &typed) {
		return typed.Kind
	}
	text := strings.ToLower(errString(err))
	switch {
	case strings.Contains(text, "score_count") || strings.Contains(text, "score count"):
		return ConflictScoreCountMismatch
	case strings.Contains(text, "running trial") || strings.Contains(text, "open trial"):
		return ConflictRunningTrials
	case strings.Contains(text, "terminal") || strings.Contains(text, "already finalized"):
		return ConflictTerminalState
	case strings.Contains(text, "immutable"):
		return ConflictImmutableField
	case strings.Contains(text, "draft"):
		return ConflictOpenDraft
	default:
		return ConflictUnknown
	}
}

type TestSuitesClientOptions struct {
	GrafanaURL          string
	ServiceAccountToken string
	ControlEndpoint     string
	RetryTimeout        time.Duration
	HTTPClient          *http.Client
}

type TestSuitesClient struct {
	endpoint   string
	grafanaURL string
	token      string
	timeout    time.Duration
	http       *http.Client
}

func NewTestSuitesClient(opts TestSuitesClientOptions) (*TestSuitesClient, error) {
	grafanaURL := firstNonBlank(opts.GrafanaURL, os.Getenv("AGENTO11Y_GRAFANA_URL"))
	endpoint := firstNonBlank(opts.ControlEndpoint, os.Getenv("AGENTO11Y_CONTROL_ENDPOINT"))
	if endpoint == "" {
		endpoint = grafanaURL
	}
	if endpoint == "" {
		return nil, errors.New("control endpoint is required: pass ControlEndpoint or set AGENTO11Y_CONTROL_ENDPOINT")
	}
	normalized, err := normalizeControlEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	token := firstNonBlank(opts.ServiceAccountToken, os.Getenv("AGENTO11Y_SERVICE_ACCOUNT_TOKEN"))
	if token == "" {
		return nil, errors.New("service account token is required: pass ServiceAccountToken or set AGENTO11Y_SERVICE_ACCOUNT_TOKEN")
	}
	timeout := opts.RetryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if grafanaURL == "" {
		parsed, _ := url.Parse(normalized)
		grafanaURL = parsed.Scheme + "://" + parsed.Host
	}
	return &TestSuitesClient{
		endpoint:   strings.TrimRight(normalized, "/"),
		grafanaURL: strings.TrimRight(grafanaURL, "/"),
		token:      token, timeout: timeout, http: httpClient,
	}, nil
}

func (c *TestSuitesClient) GrafanaURL() string {
	if c == nil {
		return ""
	}
	return c.grafanaURL
}

type ListOptions struct {
	Limit    int
	MaxPages int
}

func (c *TestSuitesClient) ListSuites(ctx context.Context, optional ...ListOptions) ([]map[string]any, error) {
	opts := normalizedListOptions(optional)
	var out []map[string]any
	cursor := ""
	for page := 0; page < opts.MaxPages; page++ {
		query := url.Values{"limit": []string{strconv.Itoa(opts.Limit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var body map[string]any
		if err := c.request(ctx, http.MethodGet, "/test-suites?"+query.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, objectItems(body)...)
		cursor = normalizeCursor(body["next_cursor"])
		if cursor == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: suite pagination did not terminate", ErrTransport)
}

func (c *TestSuitesClient) GetSuite(ctx context.Context, suiteID string) (map[string]any, error) {
	suiteID, err := requiredID(suiteID, "suite ID")
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.request(ctx, http.MethodGet, "/test-suites/"+url.PathEscape(suiteID), nil, &out)
	return out, err
}

func (c *TestSuitesClient) ListCases(ctx context.Context, suiteID, version string, optional ...ListOptions) ([]TestCase, error) {
	suiteID, err := requiredID(suiteID, "suite ID")
	if err != nil {
		return nil, err
	}
	version, err = requiredID(version, "version")
	if err != nil {
		return nil, err
	}
	opts := normalizedListOptions(optional)
	var out []TestCase
	cursor := ""
	for page := 0; page < opts.MaxPages; page++ {
		query := url.Values{"limit": []string{strconv.Itoa(opts.Limit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		path := fmt.Sprintf("/test-suites/%s/versions/%s/test-cases?%s", url.PathEscape(suiteID), url.PathEscape(version), query.Encode())
		var body map[string]any
		if err := c.request(ctx, http.MethodGet, path, nil, &body); err != nil {
			return nil, err
		}
		for _, item := range objectItems(body) {
			out = append(out, remoteCaseToLocal(item))
		}
		cursor = normalizeCursor(body["next_cursor"])
		if cursor == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: test case pagination did not terminate", ErrTransport)
}

func (c *TestSuitesClient) ResolveVersion(suite map[string]any, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("%w: version is required", ErrValidation)
	}
	versions := suiteVersions(suite)
	if requested != "latest" && requested != "latest_published" && requested != "draft" {
		for _, version := range versions {
			if stringValue(version["version"]) == requested {
				return requested, nil
			}
		}
		return "", fmt.Errorf("%w: suite version %q", ErrNotFound, requested)
	}
	var candidates []map[string]any
	for _, version := range versions {
		published, _ := version["published"].(bool)
		if requested == "latest_published" && !published {
			continue
		}
		if requested == "draft" && published {
			continue
		}
		candidates = append(candidates, version)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: suite version %q", ErrNotFound, requested)
	}
	if requested == "draft" {
		sort.SliceStable(candidates, func(i, j int) bool { return versionLess(candidates[j], candidates[i]) })
		return stringValue(candidates[0]["version"]), nil
	}
	sort.SliceStable(candidates, func(i, j int) bool { return versionLess(candidates[j], candidates[i]) })
	return stringValue(candidates[0]["version"]), nil
}

func (c *TestSuitesClient) PullSuite(ctx context.Context, suiteID, version string) (*TestSuite, error) {
	if strings.TrimSpace(version) == "" {
		version = "latest_published"
	}
	remote, err := c.GetSuite(ctx, suiteID)
	if err != nil {
		return nil, err
	}
	resolved, err := c.ResolveVersion(remote, version)
	if err != nil {
		return nil, err
	}
	cases, err := c.ListCases(ctx, suiteID, resolved)
	if err != nil {
		return nil, err
	}
	record := versionRecord(remote, resolved)
	return &TestSuite{
		SuiteID: firstNonBlank(stringValue(remote["suite_id"]), suiteID),
		Name:    stringValue(remote["name"]), Version: resolved,
		Description: stringValue(remote["description"]), Tags: stringSlice(remote["tags"]),
		Changelog: stringValue(record["changelog"]), TestCases: cases,
	}, nil
}

type PushSuiteOptions struct {
	Publish    bool
	Changelog  string
	EmptyDraft bool
	Prune      bool
}

type PushedSuite struct {
	SuiteID       string
	SuiteVersion  string
	Published     bool
	Suite         TestSuite
	RemoteSuite   map[string]any
	RemoteVersion map[string]any
	PrunedCaseIDs []string
}

func (c *TestSuitesClient) PushSuite(ctx context.Context, suite TestSuite, opts PushSuiteOptions) (*PushedSuite, error) {
	if err := validateSuite(suite); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	remote, err := c.ensureSuite(ctx, suite)
	if err != nil {
		return nil, err
	}
	if err := c.patchSuite(ctx, suite, remote); err != nil {
		return nil, err
	}
	remote, err = c.GetSuite(ctx, suite.SuiteID)
	if err != nil {
		return nil, err
	}
	version, err := c.ensureDraft(ctx, suite.SuiteID, remote, firstNonBlank(opts.Changelog, suite.Changelog), opts.EmptyDraft)
	if err != nil {
		return nil, err
	}
	versionID := stringValue(version["version"])
	if versionID == "" {
		return nil, fmt.Errorf("%w: created draft is missing version", ErrTransport)
	}
	for _, testCase := range suite.TestCases {
		payload, err := localCaseToRemote(testCase)
		if err != nil {
			return nil, err
		}
		path := fmt.Sprintf("/test-suites/%s/versions/%s/test-cases", url.PathEscape(suite.SuiteID), url.PathEscape(versionID))
		var ignored map[string]any
		if err := c.request(ctx, http.MethodPost, path, payload, &ignored); err != nil {
			return nil, err
		}
	}
	var pruned []string
	if opts.Prune {
		local := map[string]struct{}{}
		for _, testCase := range suite.TestCases {
			local[testCase.TestCaseID] = struct{}{}
		}
		remoteCases, err := c.ListCases(ctx, suite.SuiteID, versionID)
		if err != nil {
			return nil, err
		}
		for _, testCase := range remoteCases {
			if _, ok := local[testCase.TestCaseID]; ok {
				continue
			}
			path := fmt.Sprintf("/test-suites/%s/versions/%s/test-cases/%s", url.PathEscape(suite.SuiteID), url.PathEscape(versionID), url.PathEscape(testCase.TestCaseID))
			if err := c.request(ctx, http.MethodDelete, path, nil, nil); err != nil {
				return nil, err
			}
			pruned = append(pruned, testCase.TestCaseID)
		}
	}
	published := false
	if opts.Publish {
		path := fmt.Sprintf("/test-suites/%s/versions/%s:publish", url.PathEscape(suite.SuiteID), url.PathEscape(versionID))
		if err := c.request(ctx, http.MethodPost, path, nil, &version); err != nil {
			return nil, err
		}
		published, _ = version["published"].(bool)
		if _, exists := version["published"]; !exists {
			published = true
		}
	}
	pulled := *cloneSuite(&suite)
	pulled.Version = versionID
	pulled.Changelog = firstNonBlank(opts.Changelog, suite.Changelog)
	return &PushedSuite{
		SuiteID: suite.SuiteID, SuiteVersion: versionID, Published: published,
		Suite: pulled, RemoteSuite: remote, RemoteVersion: version,
		PrunedCaseIDs: pruned,
	}, nil
}

func (c *TestSuitesClient) ensureSuite(ctx context.Context, suite TestSuite) (map[string]any, error) {
	remote, err := c.GetSuite(ctx, suite.SuiteID)
	if err == nil {
		return remote, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	payload := map[string]any{"suite_id": suite.SuiteID, "name": firstNonBlank(suite.Name, suite.SuiteID)}
	if suite.Description != "" {
		payload["description"] = suite.Description
	}
	if len(suite.Tags) > 0 {
		payload["tags"] = suite.Tags
	}
	if err := c.request(ctx, http.MethodPost, "/test-suites", payload, &remote); err != nil {
		return nil, err
	}
	return remote, nil
}

func (c *TestSuitesClient) patchSuite(ctx context.Context, suite TestSuite, remote map[string]any) error {
	patch := map[string]any{}
	if suite.Name != "" && suite.Name != stringValue(remote["name"]) {
		patch["name"] = suite.Name
	}
	if suite.Description != "" {
		patch["description"] = suite.Description
	}
	if len(suite.Tags) > 0 {
		patch["tags"] = suite.Tags
	}
	if len(patch) == 0 {
		return nil
	}
	return c.request(ctx, http.MethodPatch, "/test-suites/"+url.PathEscape(suite.SuiteID), patch, nil)
}

func (c *TestSuitesClient) ensureDraft(ctx context.Context, suiteID string, suite map[string]any, changelog string, empty bool) (map[string]any, error) {
	if draft := draftVersion(suiteVersions(suite)); draft != nil {
		if err := validateDraftOptions(draft, changelog, empty); err != nil {
			return nil, err
		}
		return draft, nil
	}
	payload := map[string]any{}
	if changelog != "" {
		payload["changelog"] = changelog
	}
	if empty {
		payload["empty_draft"] = true
	}
	path := "/test-suites/" + url.PathEscape(suiteID) + "/versions"
	var out map[string]any
	if err := c.request(ctx, http.MethodPost, path, payload, &out); err == nil {
		return out, nil
	} else if !errors.Is(err, ErrConflict) {
		return nil, err
	}
	refreshed, err := c.GetSuite(ctx, suiteID)
	if err != nil {
		return nil, err
	}
	draft := draftVersion(suiteVersions(refreshed))
	if draft == nil {
		return nil, &ConflictError{Kind: ConflictOpenDraft, Message: "suite draft conflict"}
	}
	if err := validateDraftOptions(draft, changelog, empty); err != nil {
		return nil, err
	}
	return draft, nil
}

func validateDraftOptions(draft map[string]any, changelog string, empty bool) error {
	if changelog != "" && changelog != stringValue(draft["changelog"]) {
		return &ConflictError{Kind: ConflictOpenDraft, Message: "existing draft cannot apply a different changelog"}
	}
	if empty {
		return &ConflictError{Kind: ConflictOpenDraft, Message: "empty draft only applies when creating a new draft"}
	}
	return nil
}

func (c *TestSuitesClient) request(ctx context.Context, method, path string, payload any, out any) error {
	if c == nil {
		return errors.New("nil test suites client")
	}
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: encode request: %v", ErrValidation, err)
		}
	}
	var last error
	for attempt := range 6 {
		requestCtx, cancel := context.WithTimeout(contextOrBackground(ctx), c.timeout)
		req, err := http.NewRequestWithContext(requestCtx, method, c.endpoint+"/"+strings.TrimLeft(path, "/"), bytes.NewReader(data))
		if err != nil {
			cancel()
			return fmt.Errorf("%w: %v", ErrTransport, err)
		}
		req.Header.Set("Authorization", formatBearer(c.token))
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := c.http.Do(req)
		if err != nil {
			cancel()
			last = err
			if attempt < 5 {
				if err := retryWait(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%w: %v", ErrTransport, err)
		}
		body, readErr := readBounded(response.Body)
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			return fmt.Errorf("%w: %v", ErrTransport, readErr)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if out == nil || len(bytes.TrimSpace(body)) == 0 {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w: decode response: %v", ErrTransport, err)
			}
			return nil
		}
		message := responseMessage(body, response.StatusCode)
		switch response.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrValidation, message)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, message)
		case http.StatusConflict:
			err := &ConflictError{Kind: classifyConflictText(message), Message: message}
			return err
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			last = fmt.Errorf("status %d: %s", response.StatusCode, message)
			if attempt < 5 {
				if err := retryWait(ctx, attempt); err != nil {
					return err
				}
				continue
			}
		}
		return fmt.Errorf("%w: status %d: %s", ErrTransport, response.StatusCode, message)
	}
	return fmt.Errorf("%w: %v", ErrTransport, last)
}

func localCaseToRemote(testCase TestCase) (map[string]any, error) {
	if strings.TrimSpace(testCase.TestCaseID) == "" {
		return nil, fmt.Errorf("%w: test case ID is required", ErrValidation)
	}
	if testCase.Input == nil {
		return nil, fmt.Errorf("%w: test case input is required", ErrValidation)
	}
	metadata := cloneMap(testCase.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	portability := map[string]any{"version": portabilityVersion}
	var wrapped []string
	input, inputWrapped := ensureObject(testCase.Input)
	if inputWrapped {
		wrapped = append(wrapped, "input")
	}
	var expected map[string]any
	if testCase.Expected != nil {
		var expectedWrapped bool
		expected, expectedWrapped = ensureObject(testCase.Expected)
		if expectedWrapped {
			wrapped = append(wrapped, "expected")
		}
	}
	if testCase.Weight != 1 {
		portability["weight"] = testCase.Weight
	}
	if len(wrapped) > 0 {
		portability["wrapped_fields"] = wrapped
	}
	if len(portability) > 1 {
		metadata[portabilityMetadataKey] = portability
	}
	out := map[string]any{"test_case_id": testCase.TestCaseID, "input": input}
	if testCase.Name != "" {
		out["name"] = testCase.Name
	}
	if testCase.Description != "" {
		out["description"] = testCase.Description
	}
	if len(testCase.Tags) > 0 {
		out["tags"] = testCase.Tags
	}
	if testCase.Category != "" {
		out["category"] = testCase.Category
	}
	if expected != nil {
		out["expected"] = expected
	}
	if len(metadata) > 0 {
		out["metadata"] = metadata
	}
	if len(testCase.ArtifactRefs) > 0 {
		out["artifact_refs"] = testCase.ArtifactRefs
	}
	return out, nil
}

func remoteCaseToLocal(data map[string]any) TestCase {
	metadata := objectMap(data["metadata"])
	portability := objectMap(metadata[portabilityMetadataKey])
	if intValue(portability["version"]) == portabilityVersion {
		delete(metadata, portabilityMetadataKey)
	} else {
		portability = nil
	}
	weight := 1.0
	if value, ok := numberValue(portability["weight"]); ok {
		weight = value
	}
	wrapped := map[string]bool{}
	for _, field := range stringSlice(portability["wrapped_fields"]) {
		wrapped[field] = true
	}
	input, expected := data["input"], data["expected"]
	if wrapped["input"] {
		input = unwrapValue(input)
	}
	if wrapped["expected"] {
		expected = unwrapValue(expected)
	}
	return TestCase{
		TestCaseID: firstNonBlank(stringValue(data["test_case_id"]), stringValue(data["id"])),
		Name:       stringValue(data["name"]), Description: stringValue(data["description"]),
		Tags: stringSlice(data["tags"]), Category: stringValue(data["category"]),
		Input: input, Expected: expected, Weight: weight, Metadata: metadata,
		ArtifactRefs: artifactRefs(data["artifact_refs"]),
	}
}

func normalizeControlEndpoint(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(value), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("control endpoint must be an absolute URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, defaultControlPath) && !strings.HasSuffix(path, "/api/v1/eval") {
		if index := strings.Index(path, "/a/grafana-sigil-app"); index >= 0 {
			path = path[:index]
		}
		path = strings.TrimRight(path, "/") + defaultControlPath
	}
	parsed.Path, parsed.RawQuery, parsed.Fragment = path, "", ""
	return parsed.String(), nil
}

func formatBearer(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	return "Bearer " + token
}

func normalizedListOptions(optional []ListOptions) ListOptions {
	opts := ListOptions{Limit: 200, MaxPages: 100}
	if len(optional) > 0 {
		opts = optional[0]
	}
	if opts.Limit <= 0 {
		opts.Limit = 200
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = 100
	}
	return opts
}

func requiredID(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrValidation, name)
	}
	return value, nil
}

func objectItems(body map[string]any) []map[string]any {
	raw, _ := body["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func suiteVersions(suite map[string]any) []map[string]any {
	raw, _ := suite["versions"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func versionRecord(suite map[string]any, version string) map[string]any {
	for _, record := range suiteVersions(suite) {
		if stringValue(record["version"]) == version {
			return record
		}
	}
	return map[string]any{}
}

func draftVersion(versions []map[string]any) map[string]any {
	for _, version := range versions {
		if published, _ := version["published"].(bool); !published {
			return version
		}
	}
	return nil
}

var numericVersion = regexp.MustCompile(`^v(\d+)$`)

func versionLess(a, b map[string]any) bool {
	as, bs := stringValue(a["version"]), stringValue(b["version"])
	am, bm := numericVersion.FindStringSubmatch(as), numericVersion.FindStringSubmatch(bs)
	if am != nil && bm != nil {
		ai, _ := strconv.Atoi(am[1])
		bi, _ := strconv.Atoi(bm[1])
		return ai < bi
	}
	if am != nil {
		return false
	}
	if bm != nil {
		return true
	}
	return as < bs
}

func normalizeCursor(value any) string {
	text := strings.TrimSpace(stringValue(value))
	if text == "0" {
		return ""
	}
	return text
}

func ensureObject(value any) (map[string]any, bool) {
	if object, ok := value.(map[string]any); ok {
		return cloneMap(object), false
	}
	return map[string]any{"value": value}, true
}

func unwrapValue(value any) any {
	if object, ok := value.(map[string]any); ok && len(object) == 1 {
		return object["value"]
	}
	return value
}

func objectMap(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if object, ok := value.(map[string]any); ok {
		return cloneMap(object)
	}
	return map[string]any{}
}

func stringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func artifactRefs(value any) []ExperimentArtifactRef {
	raw, _ := value.([]any)
	out := make([]ExperimentArtifactRef, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, ExperimentArtifactRef{
			ArtifactID: stringValue(object["artifact_id"]), Name: stringValue(object["name"]),
			Kind: stringValue(object["kind"]), MIME: stringValue(object["mime"]),
		})
	}
	return out
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case json.Number:
		result, _ := typed.Int64()
		return int(result)
	}
	return 0
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
func retryWait(ctx context.Context, attempt int) error {
	delay := 100 * time.Millisecond * time.Duration(1<<min(attempt, 6))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-contextOrBackground(ctx).Done():
		return contextOrBackground(ctx).Err()
	}
}
func readBounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxControlResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxControlResponseBytes {
		return nil, errors.New("response too large")
	}
	return data, nil
}
func responseMessage(body []byte, status int) string {
	if text := strings.TrimSpace(string(body)); text != "" {
		return text
	}
	return fmt.Sprintf("status %d", status)
}
func classifyConflictText(text string) ConflictKind { return ClassifyConflict(errors.New(text)) }
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
