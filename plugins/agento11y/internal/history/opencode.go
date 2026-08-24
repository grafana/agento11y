package history

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
)

func init() {
	Register(AgentSpec{
		ID:          AgentOpenCode,
		DisplayName: "OpenCode",
	}, func() Importer { return &opencodeImporter{} })
}

type opencodeImporter struct {
	roots []string
}

func (o *opencodeImporter) Roots() []string {
	if len(o.roots) > 0 {
		return append([]string(nil), o.roots...)
	}
	dataDir := opencodeDataDir()
	seen := map[string]bool{}
	add := func(path string, roots *[]string) {
		path = filepath.Clean(path)
		if path != "." && !seen[path] {
			seen[path] = true
			*roots = append(*roots, path)
		}
	}

	var roots []string
	if dataDir != "" {
		add(filepath.Join(dataDir, "opencode.db"), &roots)
		if matches, err := filepath.Glob(filepath.Join(dataDir, "opencode*.db")); err == nil {
			for _, path := range matches {
				add(path, &roots)
			}
		}
	}
	if override := opencodeDBOverride(dataDir); override != "" {
		add(override, &roots)
	}
	sort.Strings(roots)
	return roots
}

func opencodeDataDir() string {
	base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode")
}

func opencodeDBOverride(dataDir string) string {
	path := strings.TrimSpace(os.Getenv("OPENCODE_DB"))
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, path)
}

func (o *opencodeImporter) Match(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") {
		return false
	}
	if override := opencodeDBOverride(opencodeDataDir()); override != "" && filepath.Clean(path) == filepath.Clean(override) {
		return true
	}
	return strings.HasPrefix(base, "opencode") && strings.HasSuffix(base, ".db")
}

// Preview is unused because OpenCode keeps every session in one database.
func (o *opencodeImporter) Preview(context.Context, string) (SessionPreview, bool, error) {
	return SessionPreview{}, false, nil
}

var opencodePlaceholderTitle = regexp.MustCompile(`^(New session - |Child session - )\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func openCodeConversationTitle(title string) string {
	title = strings.TrimSpace(title)
	if opencodePlaceholderTitle.MatchString(title) {
		return ""
	}
	return title
}

func openCodeConversationIDs(rows []sessiondb.Session) map[string]string {
	byID := make(map[string]sessiondb.Session, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		current := row.ID
		seen := map[string]bool{current: true}
		for {
			currentRow, found := byID[current]
			if !found || currentRow.ParentID == "" {
				break
			}
			if seen[currentRow.ParentID] {
				break
			}
			seen[currentRow.ParentID] = true
			current = currentRow.ParentID
		}
		out[row.ID] = current
	}
	return out
}

func (o *opencodeImporter) Previews(ctx context.Context, path string) ([]SessionPreview, error) {
	store, err := sessiondb.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	rows, err := store.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	conversationIDs := openCodeConversationIDs(rows)
	previews := make([]SessionPreview, 0, len(rows))
	for _, row := range rows {
		if row.TerminalMessageCount == 0 {
			continue
		}
		previews = append(previews, SessionPreview{
			Agent:          AgentOpenCode,
			SessionID:      row.ID,
			ConversationID: conversationIDs[row.ID],
			Title:          row.ID,
			Workspace:      row.Directory,
			SourcePath:     path,
			TurnCount:      row.TerminalMessageCount,
			SizeBytes:      row.LogicalSizeBytes,
			StartedAt:      row.CreatedAt,
			LastActivityAt: row.UpdatedAt,
		})
	}
	return previews, nil
}

func (o *opencodeImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		store, err := sessiondb.Open(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		defer func() { _ = store.Close() }()

		lineage, err := resolveOpenCodeLineage(ctx, store, sess.SessionID, sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		usedParent := map[string]bool{}
		assistantIndex := 0
		previousGenerationID := ""
		for msg, err := range store.Messages(ctx, sess.SessionID) {
			if err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			if msg.Role != "assistant" {
				continue
			}
			turnIndex := assistantIndex
			assistantIndex++
			if !opencodeTerminalAssistant(msg) {
				continue
			}
			var userParts []sessiondb.Part
			if msg.ParentID != "" && !usedParent[msg.ParentID] {
				role, found, err := store.MessageRole(ctx, msg.ParentID)
				if err != nil {
					yield(HistoricalGeneration{}, err)
					return
				}
				usedParent[msg.ParentID] = true
				if found && role == "user" {
					userParts, err = store.Parts(ctx, msg.ParentID)
					if err != nil {
						yield(HistoricalGeneration{}, err)
						return
					}
				}
			}
			parts, err := store.Parts(ctx, msg.ID)
			if err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			source := SourceRef{
				Agent:        AgentOpenCode,
				SessionID:    sess.SessionID,
				SourcePath:   sess.SourcePath,
				TurnIndex:    turnIndex,
				TurnID:       msg.ID,
				TurnIDStable: true,
			}
			parentGenerationID := previousGenerationID
			if parentGenerationID == "" {
				parentGenerationID = lineage.spawnGenerationID
			}
			gen := opencodeGeneration(sess, msg, userParts, parts, source, lineage, parentGenerationID)
			previousGenerationID = source.GenerationID()
			if !yield(gen, nil) {
				return
			}
		}
	}
}
