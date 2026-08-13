package chatstore_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/chatstore"
)

// TestLiveStores is an opt-in harness that runs the reader over the real chat
// stores on this machine. Cursor's format is undocumented and unversioned, so
// this is the only way to find out that a new release changed it.
//
// Skipped unless CURSOR_LIVE_CHATS names a directory, so it costs CI nothing.
// It reports counts and role histograms and never prints message text, which is
// what makes it safe to run against a real ~/.cursor.
//
//	CURSOR_LIVE_CHATS=~/.cursor/chats go test \
//	    ./plugins/agento11y/internal/agents/cursor/chatstore \
//	    -run TestLiveStores -v -count=1
func TestLiveStores(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("CURSOR_LIVE_CHATS"))
	if root == "" {
		t.Skip("set CURSOR_LIVE_CHATS=~/.cursor/chats to run the live store harness")
	}

	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "store.db" {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no store.db under %s", root)
	}
	sort.Strings(paths)

	var (
		stores, empty, failed   int
		messages, prompts       int
		partKinds               = map[string]int{}
		roles                   = map[string]int{}
		withModel, withCreated  int
		providerIDs, idPrefixes = 0, map[string]int{}
	)
	for _, path := range paths {
		store, err := chatstore.Open(path)
		if err != nil {
			t.Errorf("open %s: %v", path, err)
			failed++
			continue
		}
		stores++
		meta, err := store.Meta(context.Background())
		if err != nil {
			t.Errorf("meta %s: %v", path, err)
			_ = store.Close()
			continue
		}
		if meta.AgentID != filepath.Base(filepath.Dir(path)) {
			t.Errorf("%s: meta agentId %q does not match the directory", path, meta.AgentID)
		}
		if meta.LastUsedModel != "" {
			withModel++
		}
		if !meta.Created().IsZero() {
			withCreated++
		}
		rt, ok, err := store.Root(context.Background(), meta.LatestRootBlobID)
		if err != nil {
			t.Errorf("root %s: %v", path, err)
			_ = store.Close()
			continue
		}
		if !ok {
			empty++
			_ = store.Close()
			continue
		}
		n, err := store.PromptCount(context.Background(), rt.MessageIDs)
		if err != nil {
			t.Errorf("prompt count %s: %v", path, err)
		}
		prompts += n

		// The IDs are the only per-message clock the store holds, so a drop in
		// this histogram is a provider or a Cursor release taking the times away.
		// The prefix and the length are all that is logged: the rest of an ID is
		// the provider's business.
		ids, err := store.ProviderIDs(context.Background(), rt.MessageIDs)
		if err != nil {
			t.Errorf("provider IDs %s: %v", path, err)
		}
		providerIDs += len(ids)
		for _, field := range ids {
			for id := range strings.SplitSeq(field, "\n") {
				prefix, body, ok := strings.Cut(strings.TrimSpace(id), "_")
				if !ok {
					prefix = "(no prefix)"
				}
				idPrefixes[prefix+":"+strconv.Itoa(len(body))]++
			}
		}

		decoded := 0
		for msg, err := range store.Messages(context.Background(), rt.MessageIDs) {
			if err != nil {
				t.Errorf("messages %s: %v", path, err)
				break
			}
			decoded++
			roles[msg.Role]++
			for _, p := range msg.Parts {
				partKinds[msg.Role+":"+p.Type]++
			}
		}
		messages += decoded
		_ = store.Close()
	}

	t.Logf("stores=%d empty=%d failed=%d messages=%d prompts=%d withModel=%d withCreated=%d",
		stores, empty, failed, messages, prompts, withModel, withCreated)
	t.Logf("roles=%v", sortedCounts(roles))
	t.Logf("parts=%v", sortedCounts(partKinds))
	t.Logf("providerIDs=%d prefix:bodyLength=%v", providerIDs, sortedCounts(idPrefixes))
}

// sortedCounts renders a histogram most-frequent-first, so a shift in the shape
// of the data is visible in the log line without sorting it by eye.
func sortedCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + strconv.Itoa(m[k])
	}
	return out
}
