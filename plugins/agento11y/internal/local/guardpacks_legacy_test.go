package local

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyPackDefinitions(t *testing.T) {
	raw, err := guardeval.ParseRules(legacyGuardPacks)
	require.NoError(t, err)
	require.Len(t, raw, len(catalogPacks()))
	for i, old := range raw {
		for _, state := range []string{"omitted", "true", "false"} {
			t.Run(catalogPacks()[i].ID+"/"+state, func(t *testing.T) {
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(old, &fields))
				if state != "omitted" {
					fields["enabled"] = json.RawMessage(state)
				}
				input, err := json.Marshal(fields)
				require.NoError(t, err)
				upgraded := upgradeLegacyPackRules([]json.RawMessage{input})
				require.Len(t, upgraded, 1)
				current, err := packRule(catalogPacks()[i].ID)
				require.NoError(t, err)
				if state != "omitted" {
					value := state == "true"
					current.Enabled = &value
				}
				want, err := json.Marshal(current)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(upgraded[0]))
				assert.NotEqual(t, string(input), string(upgraded[0]))
				rules, errs := guardeval.DecodeRules([]json.RawMessage{input})
				require.Empty(t, errs)
				data, err := guardeval.EncodeRules(rules)
				require.NoError(t, err)
				engine := newLocalGuardsEngine("guards.toml", data, nil)
				require.Empty(t, engine.Status().Errors)
				wantCount := 1
				if state == "false" {
					wantCount = 0
				}
				assert.Equal(t, wantCount, engine.Status().Enforcing)
				saved, err := applyPackUpdates(rules, map[string]bool{})
				require.NoError(t, err)
				require.Len(t, saved, 1)
				encoded, err := json.Marshal(saved[0])
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(encoded))
			})
		}
	}
}

func TestLegacyPackLoadDoesNotWrite(t *testing.T) {
	custom := `
[[rules]]
rule_id = "custom.sibling"
phase = "postflight"
short_circuit = true
match."tags.service" = "api"
match."tags.service.name" = "backend"
transform.patterns = [{ regex = '"raw":42', replacement = '"raw":43' }]
`
	data := append(append([]byte(nil), legacyGuardPacks...), custom...)
	path := filepath.Join(t.TempDir(), "guards.toml")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	before, err := os.Stat(path)
	require.NoError(t, err)
	for range 2 {
		engine := newLocalGuardsEngine(path, data, nil)
		require.Empty(t, engine.Status().Errors)
		assert.Equal(t, 7, engine.Status().Enforcing)
		for _, tc := range []struct {
			tool, input  string
			deny, redact bool
		}{
			{"Bash", `{"command":"git reset HEAD --hard"}`, true, false},
			{"Bash", `{"command":"git -C /tmp/repo push --force"}`, true, false},
			{"Bash", `{"command":"git clean -n; echo -f"}`, false, false},
			{"Bash", `{"command":"rm '-rf' /tmp/x"}`, true, false},
			{"Bash", `{"command":"rm -r /tmp/$(basename $(pwd)) -f"}`, true, false},
			{"Bash", `{"command":"rm -- -rf"}`, false, false},
			{"Bash", `{"command":"chmod --recursive 755 /*"}`, true, false},
			{"Bash", `{"command":"chmod -R 755 \"$HOME\""}`, true, false},
			{"Bash", `{"command":"chmod -R 755 /tmp; echo /"}`, false, false},
			{"Bash", `{"command":"dd of=\"/dev/sda\""}`, true, false},
			{"Bash", `{"command":"dd if=/dev/zero of=/tmp/image bs=1 count=1; echo of=/dev/sda"}`, false, false},
			{"terminal", `{"command":"cat <.env"}`, true, false},
			{"Read", `{"file_path":".environment"}`, false, false},
			{"Bash", `{"command":"echo \u0067hp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, false, true},
		} {
			result := engine.Evaluate(packRequest(tc.tool, tc.input))
			want := agento11y.HookActionAllow
			if tc.deny {
				want = agento11y.HookActionDeny
			}
			assert.Equal(t, want, result.Action, tc.input)
			assert.Equal(t, tc.redact, result.TransformedInput != nil, tc.input)
		}
		request := packRequest("Bash", `{"raw":42}`)
		request.Context.Tags = map[string]string{"service": "api", "service.name": "backend"}
		result := engine.Evaluate(request)
		require.NotNil(t, result.TransformedInput)
		assert.Equal(t, `{"raw":43}`, string(result.TransformedInput.Output[0].Parts[0].ToolCall.InputJSON))
	}
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime())
	assert.Equal(t, before.Mode(), after.Mode())
	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, onDisk)
}

func TestLegacyPackUpgradePreservesCustomRulesAndOrder(t *testing.T) {
	raw, err := guardeval.ParseRules(legacyGuardPacks)
	require.NoError(t, err)
	for _, change := range []struct {
		name, key string
		value     any
	}{
		{"renamed", "rule_id", "custom.secret"},
		{"priority", "priority", 11},
		{"unknown field", "future", true},
		{"unknown transform field", "transform", map[string]any{"patterns": []any{}, "future": true}},
		{"malformed enabled", "enabled", "false"},
	} {
		t.Run(change.name, func(t *testing.T) {
			var fields map[string]any
			require.NoError(t, json.Unmarshal(raw[0], &fields))
			if change.name == "unknown transform field" {
				fields["transform"].(map[string]any)["future"] = true
			} else {
				fields[change.key] = change.value
			}
			custom, err := json.Marshal(fields)
			require.NoError(t, err)
			input := []json.RawMessage{custom, raw[1], json.RawMessage(`{"rule_id":"broken","priority":"bad"}`), raw[0]}
			before, err := json.Marshal(input)
			require.NoError(t, err)
			out := upgradeLegacyPackRules(input)
			require.Len(t, out, len(input))
			assert.Equal(t, string(custom), string(out[0]))
			assert.Equal(t, input[2], out[2])
			for _, i := range []int{1, 3} {
				assert.NotEqual(t, input[i], out[i])
			}
			after, err := json.Marshal(input)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestLocalPackEnginePreservesDiagnostics(t *testing.T) {
	for _, data := range [][]byte{
		nil, []byte("# empty"), []byte("[[rules]\n"), []byte(`rules = 4`),
		append(append([]byte(nil), legacyGuardPacks...), []byte("\n[[rules]]\nrule_id = 'bad'\npriority = 'twenty'\n\n[[rules]]\nrule_id = 'regex'\ntransform.patterns = [{regex = '['}]\n")...),
	} {
		want := guardeval.NewEngineFromContents("custom/path.toml", data, nil).Status()
		got := newLocalGuardsEngine("custom/path.toml", data, nil).Status()
		assert.Equal(t, want, got)
	}
}

func TestPackUpdatesPreserveEnabledState(t *testing.T) {
	legacy, err := guardeval.ParseRules(legacyGuardPacks)
	require.NoError(t, err)
	stock, err := packRule(packGit)
	require.NoError(t, err)
	current, err := json.Marshal(stock)
	require.NoError(t, err)
	on, off := true, false
	for _, source := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"current", current},
		{"legacy", legacy[2]},
	} {
		for _, change := range []struct {
			name   string
			fields map[string]any
			deny   bool
		}{
			{"stock", nil, true},
			{"edited", map[string]any{"priority": 123, "match": map[string]any{"tags.team": "platform"}, "action_on_fail": "warn"}, false},
			{"short_circuit", map[string]any{"short_circuit": true}, true},
			{"evaluator_ids", map[string]any{"evaluator_ids": []string{"ev-1"}}, true},
			{"selector", map[string]any{"selector": map[string]any{"tags.team": "platform"}}, true},
		} {
			for _, state := range []struct {
				name    string
				enabled *bool
			}{{"omitted", nil}, {"true", &on}, {"false", &off}} {
				for _, update := range []struct {
					name  string
					packs map[string]bool
				}{
					{"unchanged", nil},
					{"unrelated_on", map[string]bool{packSecrets: true}},
					{"unrelated_off", map[string]bool{packSecrets: false}},
					{"enable", map[string]bool{packGit: true}},
					{"disable", map[string]bool{packGit: false}},
				} {
					t.Run(source.name+"/"+change.name+"/"+state.name+"/"+update.name, func(t *testing.T) {
						var fields map[string]any
						require.NoError(t, json.Unmarshal(source.raw, &fields))
						maps.Copy(fields, change.fields)
						raw, err := json.Marshal(fields)
						require.NoError(t, err)
						var rule guardeval.Rule
						require.NoError(t, json.Unmarshal(raw, &rule))
						require.Equal(t, packRuleID(packGit), rule.RuleID)
						rule.Enabled = state.enabled
						wantRule := rule
						if len(change.fields) == 0 {
							wantRule = stock
							wantRule.Enabled = state.enabled
						}
						refreshed, err := refreshStoredPackRule(rule)
						require.NoError(t, err)
						assert.Equal(t, wantRule, refreshed)

						out, err := applyPackUpdates([]guardeval.Rule{rule}, update.packs)
						require.NoError(t, err)
						found := false
						for _, saved := range out {
							if saved.RuleID != rule.RuleID {
								continue
							}
							found = true
							if _, toggled := update.packs[packGit]; toggled {
								wantRule.Enabled = nil
							}
							assert.Equal(t, wantRule, saved)
						}
						wantPresent := true
						if next, toggled := update.packs[packGit]; toggled {
							wantPresent = next
						}
						assert.Equal(t, wantPresent, found)
						path := filepath.Join(t.TempDir(), "guards.toml")
						require.NoError(t, guardeval.WriteRules(path, out))
						saved, data, exists, errs, err := readGuardRules(path)
						require.NoError(t, err)
						require.True(t, exists)
						require.Empty(t, errs)
						require.Len(t, saved, len(out))
						for i := range out {
							assert.Equal(t, out[i], saved[i])
						}
						engine := newLocalGuardsEngine(path, data, nil)
						require.Empty(t, engine.Status().Errors)
						want := state.enabled == nil || *state.enabled
						if next, toggled := update.packs[packGit]; toggled {
							want = next
						}
						request := packRequest("Bash", `{"command":"git reset --hard"}`)
						request.Context.Tags = map[string]string{"team": "platform"}
						assert.Equal(t, want && change.deny, engine.Evaluate(request).Action == agento11y.HookActionDeny)
						request = packRequest("Bash", `{"command":"git reset HEAD --hard"}`)
						request.Context.Tags = map[string]string{"team": "platform"}
						wantRefreshed := source.name == "current" || len(change.fields) == 0
						assert.Equal(t, want && change.deny && wantRefreshed, engine.Evaluate(request).Action == agento11y.HookActionDeny)
					})
				}
			}
		}
	}
}
