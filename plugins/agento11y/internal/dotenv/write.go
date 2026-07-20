package dotenv

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteDotenv writes updates to the dotenv at path, preserving any existing
// allowed keys that are not in updates. Keys with empty values delete the
// corresponding entry. The file is written atomically (temp file in the
// same directory + rename) with 0600 perms, and the parent directory is
// created if missing.
//
// Comments and key ordering in the existing file are not preserved across
// rewrites; the rewritten file is sorted alphabetically. Callers that need
// to keep hand-written comments should not run this writer over their file.
//
// Only keys accepted by AllowedDotenvKey may be written; passing a disallowed
// key returns an error so a typo or unexpected caller cannot inject unrelated
// process state on next load.
func WriteDotenv(path string, updates map[string]string, logger *log.Logger) error {
	for k := range updates {
		if !AllowedDotenvKey(k) {
			return fmt.Errorf("dotenv: refusing to write disallowed key %q", k)
		}
	}

	merged := LoadDotenv(path, logger)
	for k, v := range updates {
		if strings.TrimSpace(v) == "" {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}

	rendered, err := renderDotenv(merged)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("dotenv: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("dotenv: temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(rendered); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("dotenv: write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("dotenv: chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("dotenv: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("dotenv: rename to %s: %w", path, err)
	}
	return nil
}

// RenderManaged renders updates as managed dotenv file content without
// touching disk. The output is what WriteDotenv would produce for a file that
// contained only these keys: the managed header followed by KEY=value lines
// sorted alphabetically. Empty values are dropped because they represent
// deletions, and only keys accepted by AllowedDotenvKey may appear. Callers
// use this to preview the file a form will persist without exposing any other
// keys already on disk.
func RenderManaged(updates map[string]string) ([]byte, error) {
	for k := range updates {
		if !AllowedDotenvKey(k) {
			return nil, fmt.Errorf("dotenv: refusing to render disallowed key %q", k)
		}
	}
	return renderDotenv(updates)
}

// renderDotenv serialises m into the managed dotenv format: the two-line
// managed header, then KEY=value lines sorted alphabetically, each value
// quoted by quoteDotenvValue. Keys whose value is empty (after trimming) are
// skipped so the rendered bytes match the on-disk result of deleting them.
func renderDotenv(m map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if strings.TrimSpace(v) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# Managed by `agento11y login`. Hand-edits to known keys persist\n")
	b.WriteString("# across re-runs; comments and ordering do not.\n")
	for _, k := range keys {
		quoted, err := quoteDotenvValue(m[k])
		if err != nil {
			return nil, fmt.Errorf("dotenv: cannot serialise %s: %w", k, err)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, quoted)
	}
	return []byte(b.String()), nil
}

// quoteDotenvValue returns v in a form the dotenv parser can round-trip.
// Values with no special characters are emitted raw; values with spaces,
// `#`, or quotes are wrapped in whichever quote character does not appear
// inside the value. The existing parser does not handle escape sequences,
// so a value containing both `"` and `'` cannot be represented and is
// rejected — in practice neither tokens nor URLs contain quotes.
func quoteDotenvValue(v string) (string, error) {
	if v == "" {
		return `""`, nil
	}
	if !strings.ContainsAny(v, " \t#\"'") {
		return v, nil
	}
	hasDouble := strings.ContainsRune(v, '"')
	hasSingle := strings.ContainsRune(v, '\'')
	if hasDouble && hasSingle {
		return "", fmt.Errorf("value contains both single and double quotes")
	}
	if hasDouble {
		return "'" + v + "'", nil
	}
	return `"` + v + `"`, nil
}
