package entry

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grafana/agento11y/go/agento11y/experiments"
	"github.com/grafana/agento11y/plugins/agento11y/internal/claudeevals"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

type claudeEvalFlags struct {
	name, traceRoot, expectTenant, grafanaURL string
	content, dryRun, noLocal                  bool
}

func newClaudeEvalFlags() (*flag.FlagSet, *claudeEvalFlags) {
	fs := flag.NewFlagSet("claude eval import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	f := new(claudeEvalFlags)
	fs.StringVar(&f.name, "name", "", "experiment name (default: Claude plugin eval: <plugin>)")
	fs.BoolVar(&f.content, "include-content", false, "upload redacted prompts, grader evidence, and conversation/tool content when --trace-root is set")
	fs.StringVar(&f.traceRoot, "trace-root", "", "import conversations, token usage and traces from retained out/trace.jsonl files inside this directory; requires Claude --keep-temp and OTLP configuration")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the redacted export plan as JSON without connecting or uploading")
	fs.StringVar(&f.expectTenant, "expect-tenant", "", "refuse export unless the configured tenant matches this ID")
	fs.StringVar(&f.grafanaURL, "grafana-url", "", "Grafana stack URL for experiment links (or AGENTO11Y_GRAFANA_URL)")
	fs.BoolVar(&f.noLocal, "no-local", false, "explicitly export to Cloud when AGENTO11Y_LOCAL is enabled")
	return fs, f
}

func claudeEvalHelpFlags() *flag.FlagSet {
	fs, _ := newClaudeEvalFlags()
	return fs
}

func runClaudeEvalCommand(args []string, stdout, stderr io.Writer) {
	if len(args) == 0 {
		printHelp("claude eval", stdout)
		return
	}
	if args[0] != "import" {
		fs := flag.NewFlagSet("claude eval", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Usage = func() {}
		if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
			printHelp("claude eval", stdout)
		} else if err != nil {
			usageError(stderr, "claude eval", err.Error())
			exit(2)
		} else {
			usageError(stderr, "claude eval", fmt.Sprintf("unknown claude eval verb %q", args[0]))
			exit(2)
		}
		return
	}
	fs, f := newClaudeEvalFlags()
	args = args[1:]
	path := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		path, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp("claude eval import", stdout)
			return
		}
		usageError(stderr, "claude eval import", err.Error())
		exit(2)
		return
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	} else if fs.NArg() != 0 {
		usageError(stderr, "claude eval import", fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
		exit(2)
		return
	}
	if path == "" {
		usageError(stderr, "claude eval import", "a results file is required")
		exit(2)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, "agento11y: cannot open eval result:", err)
		exit(1)
		return
	}
	plan, err := claudeevals.Parse(file, claudeevals.Options{Name: f.name, IncludeContent: f.content, TraceRoot: f.traceRoot})
	_ = file.Close()
	if err != nil {
		fmt.Fprintln(stderr, "agento11y:", err)
		exit(1)
		return
	}
	if f.dryRun {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(plan); err != nil {
			fmt.Fprintln(stderr, "agento11y:", err)
			exit(1)
		}
		return
	}
	dotenv.ApplyEnv(nil)
	if envconfig.ParseBool(envconfig.Getenv("LOCAL")) && !f.noLocal {
		fmt.Fprintln(stderr, "agento11y: eval imports require Cloud; use --no-local to explicitly override local mode")
		exit(1)
		return
	}
	if f.expectTenant != "" && envconfig.Getenv("AUTH_TENANT_ID") != f.expectTenant {
		fmt.Fprintln(stderr, "agento11y: configured tenant does not match --expect-tenant; nothing uploaded")
		exit(1)
		return
	}
	if f.grafanaURL == "" && envconfig.Getenv("GRAFANA_URL") == "" {
		f.grafanaURL = os.Getenv("AGENTO11Y_STACK_URL") // saved by agento11y login
	}
	client, err := experiments.NewClient(experiments.ClientOptions{GrafanaURL: f.grafanaURL, Actor: "ingest:claude-plugin-eval"})
	if err != nil {
		fmt.Fprintln(stderr, "agento11y:", err)
		exit(1)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var telemetry *claudeevals.Telemetry
	if plan.IncludeTrajectory {
		telemetry, err = claudeevals.NewTelemetry(ctx)
	}
	if err == nil {
		err = plan.Export(ctx, client, telemetry, stdout)
	}
	cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	err = errors.Join(err, client.Shutdown(cleanup), telemetry.Shutdown(cleanup))
	if err != nil {
		fmt.Fprintln(stderr, "agento11y: eval import:", err)
		exit(1)
	}
}
