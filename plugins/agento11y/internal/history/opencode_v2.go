package history

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
)

func opencodeV2Turns(ctx context.Context, store *sessiondb.Store, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		snapshot, err := store.V2Snapshot(ctx, sess.SessionID)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		defer func() { _ = snapshot.Close() }()
		row, _, err := snapshot.Session(ctx, sess.SessionID)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		lineage, err := resolveOpenCodeV2Lineage(ctx, snapshot, row, sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		cwd, err := opencodeV2InitialDirectory(ctx, snapshot, row)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		var pending []agento11y.Message
		pendingBytes, assistantIndex := 0, 0
		previous := lineage.spawnGenerationID
		for msg, err := range snapshot.Messages(ctx, sess.SessionID) {
			if err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			switch msg.Type {
			case "user", "synthetic":
				if strings.TrimSpace(msg.Text) == "" {
					continue
				}
				if len(msg.Text) > sessiondb.MaxPayloadBytes-pendingBytes {
					yield(HistoricalGeneration{}, fmt.Errorf("opencode session %s in %s: pending input exceeds %d bytes", sess.SessionID, sess.SourcePath, sessiondb.MaxPayloadBytes))
					return
				}
				pendingBytes += len(msg.Text)
				pending = append(pending, agento11y.UserTextMessage(msg.Text))
			case "idle":
				pending, pendingBytes = nil, 0
			case "location-switched":
				if row.ForkSessionID == "" {
					cwd = msg.Location.Directory
				}
			case "assistant":
				input := pending
				pending, pendingBytes = nil, 0
				source := opencodeV2Source(sess.SessionID, sess.SourcePath, msg.ID, assistantIndex)
				assistantIndex++
				if !msg.Terminal() {
					continue
				}
				gen := opencodeV2Generation(sess, msg, input, source, lineage, previous, cwd)
				previous = source.GenerationID()
				if !yield(gen, nil) {
					return
				}
			}
		}
	}
}

func opencodeV2Source(sessionID, path, messageID string, index int) SourceRef {
	return SourceRef{Agent: AgentOpenCode, SessionID: sessionID, SourcePath: path, TurnID: messageID, TurnIDStable: true, TurnIndex: index}
}

func opencodeV2InitialDirectory(ctx context.Context, snapshot *sessiondb.V2Snapshot, row sessiondb.Session) (string, error) {
	if row.ForkSessionID != "" {
		return "", nil
	}
	for msg, err := range snapshot.V2Messages(ctx, row.ID) {
		if err != nil {
			return "", err
		}
		if msg.Type == "location-switched" {
			if msg.Previous != nil {
				return msg.Previous.Location.Directory, nil
			}
			return "", nil
		}
	}
	return row.Directory, nil
}

func resolveOpenCodeV2Lineage(ctx context.Context, snapshot *sessiondb.V2Snapshot, row sessiondb.Session, path string) (opencodeLineage, error) {
	lineage := opencodeLineage{conversationID: row.ID, conversationTitle: openCodeConversationTitle(row.Title)}
	if row.ForkSessionID != "" || row.ParentID == "" || row.ParentID == row.ID {
		return lineage, nil
	}
	lineage.parentSessionID = row.ParentID
	current := row
	seen := map[string]bool{row.ID: true}
	for current.ParentID != "" && current.ForkSessionID == "" && !seen[current.ParentID] {
		lineage.conversationID = current.ParentID
		seen[current.ParentID] = true
		parent, found, err := snapshot.Session(ctx, current.ParentID)
		if err != nil {
			return lineage, err
		}
		if !found {
			break
		}
		current = parent
	}
	index := 0
	for msg, err := range snapshot.V2Messages(ctx, row.ParentID) {
		if err != nil {
			return lineage, err
		}
		if msg.Type != "assistant" {
			continue
		}
		source := opencodeV2Source(row.ParentID, path, msg.ID, index)
		index++
		if !msg.Terminal() {
			continue
		}
		for _, block := range msg.Content {
			if opencodeV2CreatesChild(block, row.ID) {
				lineage.spawnGenerationID = source.GenerationID()
				return lineage, nil
			}
		}
	}
	return lineage, nil
}

func opencodeV2CreatesChild(block sessiondb.V2Content, childID string) bool {
	if block.Type != "tool" {
		return false
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(block.State.Input, &input) != nil || input == nil {
		return false
	}
	switch block.Name {
	case "subagent":
		_, continuation := input["sessionID"]
		return !continuation && block.State.Metadata.SessionID == childID
	case "task":
		_, continuation := input["task_id"]
		return !continuation && block.State.Metadata.LegacySessionID == childID
	}
	return false
}
