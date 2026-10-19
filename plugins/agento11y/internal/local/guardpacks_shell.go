package local

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"

	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
)

func classifyStockShellPolicy(fields map[string]any) guardeval.ShellPolicyProfile {
	fields = maps.Clone(fields)
	if enabled, present := fields["enabled"]; present {
		if _, valid := enabled.(bool); !valid {
			return ""
		}
		delete(fields, "enabled")
	}
	id, _ := fields["rule_id"].(string)
	profiles := map[string]guardeval.ShellPolicyProfile{
		packRuleID(packGit):         guardeval.ShellPolicyGit,
		packRuleID(packDestructive): guardeval.ShellPolicyDestructive,
		packRuleID(packPermissions): guardeval.ShellPolicyPermissions,
		packRuleID(packDisk):        guardeval.ShellPolicyDisk,
	}
	profile := profiles[id]
	if profile == "" {
		return ""
	}
	pack, _ := packIDFromRule(id)
	rule, err := packRule(pack)
	if err != nil {
		return ""
	}
	data, err := json.Marshal(rule)
	if err != nil {
		return ""
	}
	var expected map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&expected) != nil || !reflect.DeepEqual(fields, expected) {
		return ""
	}
	return profile
}
