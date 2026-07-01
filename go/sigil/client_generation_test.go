package sigil

import (
	"fmt"
	"testing"
)

func TestRecordedGenerationIDsAreBounded(t *testing.T) {
	client := &Client{config: Config{GenerationExport: GenerationExportConfig{QueueSize: 1}}}
	limit := client.recordedGenerationLimit()
	for i := 0; i < limit+10; i++ {
		client.recordGenerationID(fmt.Sprintf("gen-%d", i))
	}

	if len(client.generationIDs) != limit {
		t.Fatalf("expected %d recorded generation ids, got %d", limit, len(client.generationIDs))
	}
	if client.hasRecordedGenerationID("gen-0") {
		t.Fatal("expected oldest generation id to be evicted")
	}
	if !client.hasRecordedGenerationID(fmt.Sprintf("gen-%d", limit+9)) {
		t.Fatal("expected newest generation id to remain recorded")
	}
}

func TestRecordedGenerationLimitCoversQueueAndBatch(t *testing.T) {
	client := &Client{config: Config{GenerationExport: GenerationExportConfig{
		QueueSize: 2,
		BatchSize: minRecordedGenerationIDs + 10,
	}}}
	limit := client.recordedGenerationLimit()
	want := client.config.GenerationExport.QueueSize + client.config.GenerationExport.BatchSize
	if limit != want {
		t.Fatalf("expected limit %d to cover queue plus batch, got %d", want, limit)
	}

	for i := range limit {
		client.recordGenerationID(fmt.Sprintf("gen-%d", i))
	}
	if !client.hasRecordedGenerationID("gen-0") {
		t.Fatal("expected oldest in-flight generation id to remain recorded")
	}

	client.recordGenerationID(fmt.Sprintf("gen-%d", limit))
	if client.hasRecordedGenerationID("gen-0") {
		t.Fatal("expected oldest generation id to be evicted after in-flight window")
	}
}
