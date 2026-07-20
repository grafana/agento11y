package sigiltest

import (
	"context"
	"testing"
	"time"

	sigil "github.com/grafana/agento11y/go/sigil"
)

func RecordGeneration(t testing.TB, env *Env, start sigil.GenerationStart, generation sigil.Generation, mapErr error) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("sigil test env is not initialized")
	}

	_, recorder := env.Client.StartGeneration(context.Background(), start)
	recorder.SetResult(generation, mapErr)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record generation: %v", err)
	}
}

func RecordStreamingGeneration(t testing.TB, env *Env, start sigil.GenerationStart, firstTokenAt time.Time, generation sigil.Generation, mapErr error) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("sigil test env is not initialized")
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

func RecordCallError(t testing.TB, env *Env, start sigil.GenerationStart, callErr error) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("sigil test env is not initialized")
	}

	_, recorder := env.Client.StartGeneration(context.Background(), start)
	recorder.SetCallError(callErr)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record call error: %v", err)
	}
}

func RecordEmbedding(t testing.TB, env *Env, start sigil.EmbeddingStart, result sigil.EmbeddingResult) {
	t.Helper()

	if env == nil || env.Client == nil {
		t.Fatalf("sigil test env is not initialized")
	}

	_, recorder := env.Client.StartEmbedding(context.Background(), start)
	recorder.SetResult(result)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("record embedding: %v", err)
	}
}
