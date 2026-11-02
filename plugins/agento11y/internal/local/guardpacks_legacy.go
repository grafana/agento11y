package local

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"reflect"

	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
)

// Captured by EncodeRules over catalogPacks/packRule at afa354d469c04da1cb233eb8c418535a1dab985e.
// Keep this snapshot separate from the current catalog for exact-match upgrades.
//
//go:embed testdata/guardpacks/afa354d4.toml
var legacyGuardPacks []byte

// Captured the same way at plugins/agento11y/v0.48.0 (the packs of v0.47.0 and
// v0.48.0) and v0.52.0 (v0.49.0 to v0.52.0). afa354d4 holds the packs of
// v0.45.0 and v0.46.0. A pack rule saved by any of these releases and never
// edited is upgraded to the current pack.
var (
	//go:embed testdata/guardpacks/v0.48.0.toml
	releasedGuardPacksV048 []byte
	//go:embed testdata/guardpacks/v0.52.0.toml
	releasedGuardPacksV052 []byte

	releasedGuardPacks = [][]byte{legacyGuardPacks, releasedGuardPacksV048, releasedGuardPacksV052}
)

// NewGuardsEngineFromContents upgrades unchanged legacy packs in memory before compilation.
// It does not read or write path; path only identifies file diagnostics.
func NewGuardsEngineFromContents(path string, data []byte, logger *log.Logger) *guardeval.Engine {
	raw, err := guardeval.ParseRules(data)
	if err != nil {
		return guardeval.NewEngineFromContents(path, data, logger)
	}
	return guardeval.NewRulesEngineWithShellPolicies(upgradeLegacyPackRules(raw), logger, classifyStockShellPolicy)
}

func refreshStoredPackRule(rule guardeval.Rule) (guardeval.Rule, error) {
	// Compare original fields too, so nested metadata prevents a stock refresh.
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	if err != nil {
		return rule, err
	}
	raw, err := guardeval.ParseRules(data)
	if err != nil {
		return rule, err
	}
	updated := upgradeLegacyPackRules(raw)
	if bytes.Equal(raw[0], updated[0]) {
		return rule, nil
	}
	rules, errs := guardeval.DecodeRules(updated)
	if len(errs) > 0 {
		return rule, errors.Join(errs...)
	}
	return rules[0], nil
}

func upgradeLegacyPackRules(raw []json.RawMessage) []json.RawMessage {
	var legacy []json.RawMessage
	for _, snapshot := range releasedGuardPacks {
		rules, err := guardeval.ParseRules(snapshot)
		if err != nil {
			return raw
		}
		legacy = append(legacy, rules...)
	}
	out := append([]json.RawMessage(nil), raw...)
	for i, item := range raw {
		var fields map[string]any
		if json.Unmarshal(item, &fields) != nil {
			continue
		}
		enabled, present := fields["enabled"]
		if present {
			if _, valid := enabled.(bool); !valid {
				continue
			}
		}
		delete(fields, "enabled")
		for _, previous := range legacy {
			var expected map[string]any
			if json.Unmarshal(previous, &expected) != nil || !reflect.DeepEqual(fields, expected) {
				continue
			}
			id, ok := fields["rule_id"].(string)
			if !ok {
				continue
			}
			pack, ok := packIDFromRule(id)
			if !ok {
				continue
			}
			rule, err := packRule(pack)
			if err != nil {
				continue
			}
			if present {
				value := enabled.(bool)
				rule.Enabled = &value
			}
			updated, err := json.Marshal(rule)
			if err == nil {
				out[i] = updated
			}
			break
		}
	}
	return out
}
