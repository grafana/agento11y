package entry

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/harbor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/cli"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
)

// harborRun is a test seam so dispatch tests do not need the Python bridge.
var harborRun = harbor.Run

// runHarborCommand runs `agento11y harbor ...`, logging in first when no
// Grafana Cloud credentials are configured.
func runHarborCommand(args []string, stderr io.Writer) {
	dotenv.ApplyEnv(nil)
	if !dotenv.HasCredentials() {
		logger, closeLog := cli.InitLogger("harbor")
		_, err := loginRun(context.Background(), login.RunOpts{
			Stderr:     stderr,
			Logger:     logger,
			OfferLocal: false,
		})
		closeLog()
		if err != nil && !errors.Is(err, login.ErrNotInteractive) {
			_, _ = fmt.Fprintf(stderr, "agento11y: setup failed: %v\n", err)
			exit(1)
			return
		}
		dotenv.ApplyEnv(nil)
	}
	if err := harborRun(args, stderr); err != nil {
		exitAfterLaunchError(stderr, err)
	}
}

// routeHarborHelp prints help only for a bare --help. Every other argument
// belongs to the Harbor CLI, so it's passed through.
func routeHarborHelp(path string, rest []string, stdout io.Writer) bool {
	if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h") {
		printHelp(path, stdout)
		return true
	}
	return false
}
