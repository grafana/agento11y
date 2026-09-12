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

func runClaudeEvalCommand(args []string, stdout, stderr io.Writer) {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprintln(stderr, "usage: agento11y claude eval import <results.json> [flags]")
		exit(2)
		return
	}
	fs := flag.NewFlagSet("claude eval import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "experiment name (default: Claude plugin eval: <plugin>)")
	content := fs.Bool("include-content", false, "upload redacted prompts, grader evidence, and conversation/tool content when --trace-root is set")
	traceRoot := fs.String("trace-root", "", "import conversations, token usage and traces from retained out/trace.jsonl files inside this directory; requires Claude --keep-temp and OTLP configuration")
	dryRun := fs.Bool("dry-run", false, "print the redacted export plan as JSON without connecting or uploading")
	expectTenant := fs.String("expect-tenant", "", "refuse export unless the configured tenant matches this ID")
	grafanaURL := fs.String("grafana-url", "", "Grafana stack URL for experiment links (or AGENTO11Y_GRAFANA_URL)")
	noLocal := fs.Bool("no-local", false, "explicitly export to Cloud when AGENTO11Y_LOCAL is enabled")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: agento11y claude eval import <results.json> [flags]")
		fs.PrintDefaults()
	}
	args = args[1:]
	path := ""
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		path, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			exit(2)
		}
		return
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fs.Usage()
		exit(2)
		return
	}
	if path == "" {
		fs.Usage()
		exit(2)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(stderr, "agento11y: cannot open eval result:", err)
		exit(1)
		return
	}
	plan, err := claudeevals.Parse(file, claudeevals.Options{Name: *name, IncludeContent: *content, TraceRoot: *traceRoot})
	_ = file.Close()
	if err != nil {
		fmt.Fprintln(stderr, "agento11y:", err)
		exit(1)
		return
	}
	if *dryRun {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(plan); err != nil {
			fmt.Fprintln(stderr, "agento11y:", err)
			exit(1)
		}
		return
	}
	dotenv.ApplyEnv(nil)
	if envconfig.ParseBool(envconfig.Getenv("LOCAL")) && !*noLocal {
		fmt.Fprintln(stderr, "agento11y: eval imports require Cloud; use --no-local to explicitly override local mode")
		exit(1)
		return
	}
	if *expectTenant != "" && envconfig.Getenv("AUTH_TENANT_ID") != *expectTenant {
		fmt.Fprintln(stderr, "agento11y: configured tenant does not match --expect-tenant; nothing uploaded")
		exit(1)
		return
	}
	if *grafanaURL == "" && envconfig.Getenv("GRAFANA_URL") == "" {
		*grafanaURL = os.Getenv("AGENTO11Y_STACK_URL") // saved by agento11y login
	}
	client, err := experiments.NewClient(experiments.ClientOptions{GrafanaURL: *grafanaURL, Actor: "ingest:claude-plugin-eval"})
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
