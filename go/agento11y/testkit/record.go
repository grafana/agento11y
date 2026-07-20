package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

func RecordGeneration(t testing.TB, env *Env, start agento11y.GenerationStart, generation agento11y.Generation, mapErr error) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("agento11y test env is not initialized")
	}

	_, recorder := env.Client.StartGeneration(context.Background(), start)
	recorder.SetResult(generation, mapErr)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record generation: %v", err)
	}
}

func RecordStreamingGeneration(t testing.TB, env *Env, start agento11y.GenerationStart, firstTokenAt time.Time, generation agento11y.Generation, mapErr error) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("agento11y test env is not initialized")
	}

	_, recorder := env.Client.StartStreamingGeneration(context.Background(), start)
	if !firstTokenAt.IsZero() {
		recorder.SetFirstTokenAt(firstTokenAt)
	}
	recorder.SetResult(generation, mapErr)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record streaming generation: %v", err)
	}
}

func RecordCallError(t testing.TB, env *Env, start agento11y.GenerationStart, callErr error) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("agento11y test env is not initialized")
	}

	_, recorder := env.Client.StartGeneration(context.Background(), start)
	recorder.SetCallError(callErr)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record call error: %v", err)
	}
}

func RecordEmbedding(t testing.TB, env *Env, start agento11y.EmbeddingStart, result agento11y.EmbeddingResult) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("agento11y test env is not initialized")
	}

	_, recorder := env.Client.StartEmbedding(context.Background(), start)
	recorder.SetResult(result)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record embedding: %v", err)
	}
}
