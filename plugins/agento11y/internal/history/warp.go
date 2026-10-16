package history

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/warp/warpdb"
)

func init() {
	Register(AgentSpec{
		ID:          AgentWarp,
		DisplayName: "Warp",
	}, func() Importer { return &warpImporter{} })
}

const warpDatabaseName = "warp.sqlite"

type warpImporter struct {
	roots []string
}

func (w *warpImporter) Roots() []string {
	if len(w.roots) > 0 {
		return append([]string(nil), w.roots...)
	}
	home, _ := os.UserHomeDir()
	return warpDatabasePaths(runtime.GOOS, home, os.Getenv("XDG_STATE_HOME"))
}

// warpDatabasePaths lists every warp.sqlite location for the Stable and Preview
// channels. The Warp app and the Warp Agent CLI (stored under tui/) keep
// separate databases. On macOS the app moved its state into the App Group
// container; the older Application Support location is still read.
func warpDatabasePaths(goos, home, xdgStateHome string) []string {
	var dirs []string
	switch goos {
	case "darwin":
		if home == "" {
			return nil
		}
		bases := []string{
			filepath.Join(home, "Library", "Group Containers", "2BBY89MBSN.dev.warp", "Library", "Application Support"),
			filepath.Join(home, "Library", "Application Support"),
		}
		for _, base := range bases {
			for _, bundle := range []string{"dev.warp.Warp-Stable", "dev.warp.Warp-Preview"} {
				dirs = append(dirs, filepath.Join(base, bundle))
			}
		}
	case "windows":
		return nil
	default:
		state := strings.TrimSpace(xdgStateHome)
		if state == "" {
			if home == "" {
				return nil
			}
			state = filepath.Join(home, ".local", "state")
		}
		for _, name := range []string{"warp-terminal", "warp-terminal-preview"} {
			dirs = append(dirs, filepath.Join(state, name))
		}
	}
	paths := make([]string, 0, len(dirs)*2)
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(dir, warpDatabaseName), filepath.Join(dir, "tui", warpDatabaseName))
	}
	return paths
}

func (w *warpImporter) Match(path string) bool {
	return filepath.Base(path) == warpDatabaseName
}

// Preview is unused because Warp keeps every conversation in one database.
func (w *warpImporter) Preview(context.Context, string) (SessionPreview, bool, error) {
	return SessionPreview{}, false, nil
}

func (w *warpImporter) Previews(ctx context.Context, path string) ([]SessionPreview, error) {
	store, err := warpdb.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	convs, err := store.Conversations(ctx)
	if err != nil {
		return nil, err
	}
	previews := make([]SessionPreview, 0, len(convs))
	for _, conv := range convs {
		if conv.Requests == 0 || conv.Oversized {
			continue
		}
		previews = append(previews, SessionPreview{
			Agent:          AgentWarp,
			SessionID:      conv.ID,
			Title:          conv.ID,
			Workspace:      conv.Workspace,
			SourcePath:     path,
			TurnCount:      conv.Requests,
			SizeBytes:      conv.SizeBytes,
			StartedAt:      conv.StartedAt,
			LastActivityAt: conv.UpdatedAt,
		})
	}
	return previews, nil
}

func (w *warpImporter) Turns(ctx context.Context, sess SessionPreview) iter.Seq2[HistoricalGeneration, error] {
	return func(yield func(HistoricalGeneration, error) bool) {
		store, err := warpdb.Open(sess.SourcePath)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		defer func() { _ = store.Close() }()

		tasks, err := store.Tasks(ctx, sess.SessionID)
		if err != nil {
			yield(HistoricalGeneration{}, err)
			return
		}
		conv := newWarpConversation(tasks)
		previousGenerationID := ""
		for i, turn := range warpRequests(tasks) {
			if err := ctx.Err(); err != nil {
				yield(HistoricalGeneration{}, err)
				return
			}
			source := SourceRef{
				Agent:        AgentWarp,
				SessionID:    sess.SessionID,
				SourcePath:   sess.SourcePath,
				TurnIndex:    i,
				TurnID:       turn.requestID,
				TurnIDStable: true,
			}
			gen := warpGeneration(sess, turn, source, conv, previousGenerationID)
			previousGenerationID = source.GenerationID()
			if !yield(gen, nil) {
				return
			}
		}
	}
}
