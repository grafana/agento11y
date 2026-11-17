package skilldetect

import (
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skillidentity"
)

// SlashCommandIDPrefix marks the pending tool that stands for a skill the user
// invoked as a slash command. Claude Code writes no Skill tool_use for it, so
// the command record's transcript UUID supplies the call ID.
const SlashCommandIDPrefix = "slash:"

// skillBodyPrefix starts the meta line Claude Code expands from a SKILL.md
// skill. Plain custom commands and built-in prompts are expanded without it.
const skillBodyPrefix = "Base directory for this skill:"

// SlashCommand returns the validated skill name of a slash command record that
// Claude Code wrote for a prompt the user typed, for example
// "<command-name>/openspec-explore</command-name>" yields "openspec-explore".
// The record alone does not prove a skill ran: built-in local commands such as
// /model write the same record. Callers confirm it with the expanded body or
// Claude Code's own attribution before reporting a skill load.
func SlashCommand(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<command-name>") && !strings.HasPrefix(t, "<command-message>") {
		return "", false
	}
	name := transcript.TagText(t, "command-name")
	if !strings.HasPrefix(name, "/") {
		return "", false
	}
	return skillidentity.Validate(strings.TrimPrefix(name, "/"))
}

// SlashCommandInBlocks finds a slash command record in a user line. The record
// can follow other text blocks: the IDE extensions send the open file or the
// selection (<ide_opened_file>, <ide_selection>) ahead of the typed prompt.
func SlashCommandInBlocks(text string, blocks []transcript.UserContentBlock) (string, bool) {
	if text != "" {
		return SlashCommand(text)
	}
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		if name, ok := SlashCommand(block.Text); ok {
			return name, true
		}
	}
	return "", false
}

// SkillBody reports whether an expanded meta line is the body of a SKILL.md
// skill. Only the structural prefix is read; the body is never kept.
func SkillBody(text string, blocks []transcript.UserContentBlock) bool {
	if text != "" {
		return strings.HasPrefix(strings.TrimSpace(text), skillBodyPrefix)
	}
	for _, block := range blocks {
		if block.Type == "text" {
			return strings.HasPrefix(strings.TrimSpace(block.Text), skillBodyPrefix)
		}
	}
	return false
}
