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

func newLocalGuardsEngine(path string, data []byte, logger *log.Logger) *guardeval.Engine {
	raw, err := guardeval.ParseRules(data)
	if err != nil {
		return guardeval.NewEngineFromContents(path, data, logger)
	}
	return guardeval.NewRulesEngine(upgradeLegacyPackRules(raw), logger)
}

func refreshStoredPackRule(rule guardeval.Rule) (guardeval.Rule, error) {
	// EncodeRules includes metadata that json.Marshal omits from Rule.extra.
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
	legacy, err := guardeval.ParseRules(legacyGuardPacks)
	if err != nil {
		return raw
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
