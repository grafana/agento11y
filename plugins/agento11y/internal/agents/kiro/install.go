// Package kiro implements experimental Kiro CLI 3 capture using workspace hooks.
package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/execpath"
	"github.com/grafana/agento11y/plugins/agento11y/internal/fragmentstore"
	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

var lookPath = exec.LookPath
var execFn = syscall.Exec
var versionOutput = func(ctx context.Context, bin string) ([]byte, error) { return launcher.Output(ctx, bin, "--version") }
var versionPattern = regexp.MustCompile(`(?:^|\s)v?([0-9]+)\.[0-9]+`)

var triggers = []string{"UserPromptSubmit", "PostToolUse", "Stop", "SessionEnd"}

type action struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}
type hookSpec struct {
	Name    string `json:"name"`
	Trigger string `json:"trigger"`
	Action  action `json:"action"`
	Timeout int    `json:"timeout"`
}
type hookFile struct {
	Version string     `json:"version"`
	Hooks   []hookSpec `json:"hooks"`
}

func configPath() (string, error) {
	cwd, err := os.Getwd()
	return filepath.Join(cwd, ".kiro", "hooks", "agento11y.json"), err
}

func desired() (hookFile, error) {
	command, err := execpath.HookCommand("kiro hook")
	cfg := hookFile{Version: "v1"}
	for _, trigger := range triggers {
		cfg.Hooks = append(cfg.Hooks, hookSpec{Name: "agento11y-" + trigger, Trigger: trigger, Action: action{Type: "command", Command: command}, Timeout: 30})
	}
	return cfg, err
}

// owned refuses unknown fields and customized hooks rather than deleting user work.
func owned(raw []byte) bool {
	var cfg hookFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&cfg) != nil || cfg.Version != "v1" || len(cfg.Hooks) != len(triggers) {
		return false
	}
	for i, h := range cfg.Hooks {
		if h.Name != "agento11y-"+triggers[i] || h.Trigger != triggers[i] || h.Action.Type != "command" || !strings.HasSuffix(h.Action.Command, " kiro hook") || h.Timeout != 30 {
			return false
		}
	}
	return dec.Decode(new(any)) == io.EOF
}

// Install writes only the integration's workspace file; other hook files are untouched.
func Install() (bool, error) {
	path, err := configPath()
	if err != nil {
		return false, err
	}
	cfg, err := desired()
	if err != nil {
		return false, err
	}
	want, err := json.Marshal(cfg)
	if err != nil {
		return false, err
	}
	return installFile(path, want, cfg)
}

func installFile(path string, want []byte, cfg hookFile) (changed bool, err error) {
	err = fragmentstore.WithFileLock(path, func() error {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			var compact bytes.Buffer
			if json.Compact(&compact, raw) == nil && bytes.Equal(compact.Bytes(), want) {
				return nil
			}
			if !owned(raw) {
				return fmt.Errorf("refusing to replace customized Kiro hooks at %s", path)
			}
		}
		if err := fragmentstore.WriteJSON(path, cfg); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return
}

// Uninstall removes only an unmodified agento11y hook file in the current workspace.
func Uninstall() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	return fragmentstore.WithFileLock(path, func() error {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !owned(raw) {
			return fmt.Errorf("refusing to remove customized Kiro hooks at %s", path)
		}
		return os.Remove(path)
	})
}

// Status checks capture in the current workspace without changing it.
func Status(context.Context) (bool, string, error) {
	path, err := configPath()
	if err != nil {
		return false, "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	cfg, err := desired()
	if err != nil {
		return false, "", err
	}
	var got hookFile
	if err := json.Unmarshal(raw, &got); err != nil {
		return false, "", err
	}
	want, _ := json.Marshal(cfg)
	actual, _ := json.Marshal(got)
	return owned(raw) && bytes.Equal(want, actual), "", nil
}

// Launch installs workspace hooks and replaces the process with kiro-cli.
// CLI 2.x uses a different hook schema; callers must use CLI 3 or newer.
func Launch(ctx context.Context, args []string, localEnv *local.LaunchEnv, _ io.Reader, _, stderr io.Writer, _ *log.Logger, _ string) error {
	bin, err := lookPath("kiro-cli")
	if err != nil {
		return fmt.Errorf("kiro-cli not found on PATH: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := versionOutput(probeCtx, bin)
	match := versionPattern.FindStringSubmatch(string(output))
	if err != nil || len(match) != 2 {
		return fmt.Errorf("cannot verify Kiro CLI version; CLI 3 or newer is required")
	}
	major, err := strconv.Atoi(match[1])
	if err != nil || major < 3 {
		return fmt.Errorf("Kiro CLI 3 or newer is required for workspace hooks")
	}
	if _, err := Install(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stderr, "agento11y: experimental Kiro CLI 3 capture (prompts and tools; no assistant text or token usage)")
	return execFn(bin, append([]string{bin}, args...), local.Environ(localEnv))
}
