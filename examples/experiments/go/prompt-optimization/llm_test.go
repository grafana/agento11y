package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLLMRequestShapes(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("request path = %q, want /v1/chat/completions", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, body)
		response.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(response, `{
			"choices":[{"message":{"role":"assistant","content":"{\"prompts\":[{\"prompt\":[{\"role\":\"system\",\"content\":\"candidate\"}]}]}"}}],
			"usage":{"prompt_tokens":12,"completion_tokens":3}
		}`)
	}))
	t.Cleanup(server.Close)

	client, err := newLLMClient(server.URL+"/v1", "test-key", server.Client())
	if err != nil {
		t.Fatalf("newLLMClient() error = %v", err)
	}
	if _, _, err := callTask(context.Background(), client, "task-model", []chatMessage{
		{Role: "system", Content: "prompt"},
	}); err != nil {
		t.Fatalf("callTask() error = %v", err)
	}
	if _, err := proposeCandidates(context.Background(), client, "prompt", 0.5, 1, "reasoning-model"); err != nil {
		t.Fatalf("proposeCandidates() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}

	task := requests[0]
	if got := task["temperature"]; got != float64(0) {
		t.Errorf("task temperature = %#v, want 0", got)
	}
	if got := task["max_tokens"]; got != float64(32) {
		t.Errorf("task max_tokens = %#v, want 32", got)
	}
	if got := task["model"]; got != "task-model" {
		t.Errorf("task model = %#v, want task-model", got)
	}

	reasoning := requests[1]
	if _, exists := reasoning["temperature"]; exists {
		t.Errorf("reasoning temperature = %#v, want field omitted", reasoning["temperature"])
	}
	if got := reasoning["max_tokens"]; got != float64(4096) {
		t.Errorf("reasoning max_tokens = %#v, want 4096", got)
	}
	if got := reasoning["model"]; got != "reasoning-model" {
		t.Errorf("reasoning model = %#v, want reasoning-model", got)
	}
}
