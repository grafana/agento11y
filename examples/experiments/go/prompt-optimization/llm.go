package main

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
	"strings"
	"time"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type tokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage tokenUsage `json:"usage"`
}

type llmClient struct {
	endpoint   *url.URL
	apiKey     string
	httpClient *http.Client
}

func newLLMClient(baseURL, apiKey string, httpClient *http.Client) (*llmClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse LLM base URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("LLM base URL must use http or https")
	}
	if base.Host == "" {
		return nil, errors.New("LLM base URL must include a host")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	endpoint := base.ResolveReference(&url.URL{Path: "chat/completions"})
	if apiKey == "" {
		apiKey = firstNonEmpty(os.Getenv("OPENAI_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"), "not-needed")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 120 * time.Second}
	}
	return &llmClient{endpoint: endpoint, apiKey: apiKey, httpClient: httpClient}, nil
}

func (c *llmClient) chat(ctx context.Context, request chatRequest) (string, tokenUsage, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("encode chat completion request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("create chat completion request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)

	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("call chat completion endpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		if readErr != nil {
			return "", tokenUsage{}, fmt.Errorf("chat completion returned %s", response.Status)
		}
		return "", tokenUsage{}, fmt.Errorf("chat completion returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	var payload chatResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", tokenUsage{}, fmt.Errorf("decode chat completion response: %w", err)
	}
	if len(payload.Choices) == 0 {
		return "", tokenUsage{}, errors.New("chat completion response contained no choices")
	}
	return payload.Choices[0].Message.Content, payload.Usage, nil
}
