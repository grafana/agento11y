package entry

import (
	"fmt"
	"io"

	"github.com/grafana/agento11y/plugins/agento11y/internal/clihelp"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
)

// runSkillsCommand dispatches `agento11y skills <verb>`. The skills ship
// inside the binary, so no verb here reads the filesystem or the network. The
// verbs come from internal/skills, which also owns the hints that print them.
func runSkillsCommand(args []string, stdout, stderr io.Writer) {
	if routeHelp(append([]string{"skills"}, args...), stdout, stderr) {
		return
	}
	switch args[0] {
	case skills.ListVerb:
		all := skills.All()
		rows := make([]clihelp.Row, 0, len(all))
		for _, skill := range all {
			rows = append(rows, clihelp.Row{Name: skill.Name, Description: skill.Description})
		}
		clihelp.New(stdout).Rows(rows)
	case skills.ShowVerb, skills.GetVerb:
		if len(args) != 2 {
			usageError(stderr, "skills "+args[0], "expected one skill name")
			exit(2)
			return
		}
		skill, err := skills.Get(args[1])
		if err != nil {
			usageError(stderr, "skills "+args[0], fmt.Sprintf("%v; run `agento11y skills list` to see bundled skills", err))
			exit(2)
			return
		}
		_, _ = io.WriteString(stdout, skill.Body)
	default:
		usageError(stderr, "skills", fmt.Sprintf("unknown skills verb %q", args[0]))
		exit(2)
	}
}
