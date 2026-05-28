package sigil

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil/sigilmodel"
)

type ArtifactKind = sigilmodel.ArtifactKind

const (
	ArtifactKindRequest       = sigilmodel.ArtifactKindRequest
	ArtifactKindResponse      = sigilmodel.ArtifactKindResponse
	ArtifactKindTools         = sigilmodel.ArtifactKindTools
	ArtifactKindProviderEvent = sigilmodel.ArtifactKindProviderEvent
)

type Artifact = sigilmodel.Artifact

type ArtifactRef = sigilmodel.ArtifactRef

type Record struct {
	ID          string
	Kind        ArtifactKind
	Name        string
	ContentType string
	Payload     []byte
	CreatedAt   time.Time
}

type RecordStore interface {
	Put(ctx context.Context, record Record) (string, error)
}

func NewJSONArtifact(kind ArtifactKind, name string, value any) (Artifact, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return Artifact{}, err
	}

	return Artifact{
		Kind:        kind,
		Name:        name,
		ContentType: "application/json",
		Payload:     payload,
	}, nil
}

type MemoryRecordStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

func NewMemoryRecordStore() *MemoryRecordStore {
	return &MemoryRecordStore{
		records: map[string]Record{},
	}
}

func (s *MemoryRecordStore) Put(_ context.Context, record Record) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := record.ID
	if id == "" {
		id = newRandomID("rec")
	}

	record.ID = id
	record.Payload = append([]byte(nil), record.Payload...)

	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	s.records[id] = record
	return id, nil
}

func (s *MemoryRecordStore) Get(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.records[id]
	if !ok {
		return Record{}, false
	}

	record.Payload = append([]byte(nil), record.Payload...)
	return record, true
}

func (s *MemoryRecordStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.records)
}
