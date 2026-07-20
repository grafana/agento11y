package agento11y

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (c *Client) requestEvalJSON(ctx context.Context, method, endpoint string, headers map[string]string, payload any, out any, transportSentinel error, label string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := c.experimentRetryPolicy()
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: marshal %s request: %v", transportSentinel, label, err)
		}
	}

	attempt := 0
	backoff := policy.initialBackoff
	for {
		reqCtx := ctx
		var cancel context.CancelFunc
		if policy.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, policy.timeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(data))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return fmt.Errorf("%w: build %s request: %v", transportSentinel, label, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}

		httpResp, err := http.DefaultClient.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if attempt < policy.maxRetries {
				if sleepBackoff(ctx, backoff) != nil {
					return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
				}
				attempt++
				backoff = nextBackoff(backoff, policy)
				continue
			}
			return fmt.Errorf("%w: %s request: %v", transportSentinel, label, err)
		}

		body, readErr := readLimitedBody(httpResp.Body)
		_ = httpResp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if readErr != nil {
			return fmt.Errorf("%w: read %s response: %v", transportSentinel, label, readErr)
		}
		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			if len(strings.TrimSpace(string(body))) == 0 {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w: decode %s response: %v", transportSentinel, label, err)
			}
			return nil
		}

		bodyText := responseErrorText(body, httpResp.StatusCode)
		switch httpResp.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			if label == "score export" {
				return fmt.Errorf("%w: %s", ErrScoreValidationFailed, bodyText)
			}
			return fmt.Errorf("%w: %s", ErrExperimentValidationFailed, bodyText)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrExperimentNotFound, bodyText)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrExperimentConflict, bodyText)
		}
		if (httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= http.StatusInternalServerError) && attempt < policy.maxRetries {
			if sleepBackoff(ctx, backoff) != nil {
				return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
			}
			attempt++
			backoff = nextBackoff(backoff, policy)
			continue
		}
		return fmt.Errorf("%w: status %d: %s", transportSentinel, httpResp.StatusCode, bodyText)
	}
}

func (c *Client) requestEvalBytesJSON(ctx context.Context, method, endpoint string, headers map[string]string, payload []byte, out any, transportSentinel error, label string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := c.experimentRetryPolicy()
	attempt := 0
	backoff := policy.initialBackoff
	for {
		reqCtx := ctx
		var cancel context.CancelFunc
		if policy.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, policy.timeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return fmt.Errorf("%w: build %s request: %v", transportSentinel, label, err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		httpResp, err := http.DefaultClient.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if attempt < policy.maxRetries {
				if sleepBackoff(ctx, backoff) != nil {
					return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
				}
				attempt++
				backoff = nextBackoff(backoff, policy)
				continue
			}
			return fmt.Errorf("%w: %s request: %v", transportSentinel, label, err)
		}
		body, readErr := readLimitedBody(httpResp.Body)
		_ = httpResp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if readErr != nil {
			return fmt.Errorf("%w: read %s response: %v", transportSentinel, label, readErr)
		}
		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			if len(strings.TrimSpace(string(body))) == 0 || out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w: decode %s response: %v", transportSentinel, label, err)
			}
			return nil
		}
		bodyText := responseErrorText(body, httpResp.StatusCode)
		switch httpResp.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrExperimentValidationFailed, bodyText)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrExperimentNotFound, bodyText)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrExperimentConflict, bodyText)
		}
		if (httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= http.StatusInternalServerError) && attempt < policy.maxRetries {
			if sleepBackoff(ctx, backoff) != nil {
				return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
			}
			attempt++
			backoff = nextBackoff(backoff, policy)
			continue
		}
		return fmt.Errorf("%w: status %d: %s", transportSentinel, httpResp.StatusCode, bodyText)
	}
}

func experimentsURL(endpoint string, insecure bool, pathPrefix string) (string, error) {
	base, err := baseURLFromAPIEndpoint(endpoint, insecure)
	if err != nil {
		return "", err
	}
	prefix := strings.Trim(strings.TrimSpace(pathPrefix), "/")
	if prefix == "" {
		prefix = strings.Trim(defaultEvalPathPrefix, "/")
	}
	return strings.TrimRight(base, "/") + "/" + prefix + evalExperimentsSuffix, nil
}

func experimentURL(endpoint string, insecure bool, pathPrefix string, runID string) (string, error) {
	normalized := strings.TrimSpace(runID)
	if normalized == "" {
		return "", errors.New("run_id is required")
	}
	base, err := experimentsURL(endpoint, insecure, pathPrefix)
	if err != nil {
		return "", err
	}
	return base + "/" + url.PathEscape(normalized), nil
}

func readLimitedBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, int64(maxEvalResponseBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxEvalResponseBytes {
		return nil, errors.New("response too large")
	}
	return raw, nil
}

func responseErrorText(body []byte, status int) string {
	text := strings.TrimSpace(string(body))
	if text != "" {
		return text
	}
	return fmt.Sprintf("status %d", status)
}

func sleepBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nextBackoff(current time.Duration, policy experimentRetryPolicy) time.Duration {
	next := current * 2
	if current <= 0 {
		next = policy.initialBackoff
	}
	if policy.maxBackoff > 0 && next > policy.maxBackoff {
		return policy.maxBackoff
	}
	return next
}
