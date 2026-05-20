package config

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/copilot/internal/util"
)

type Config struct {
	ContentCapture sigil.ContentCaptureMode
	Debug          bool
}

func HasCredentials() bool {
	return strings.TrimSpace(os.Getenv("SIGIL_ENDPOINT")) != "" &&
		strings.TrimSpace(os.Getenv("SIGIL_AUTH_TENANT_ID")) != "" &&
		strings.TrimSpace(os.Getenv("SIGIL_AUTH_TOKEN")) != ""
}

func FilePath() string {
	if x := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); x != "" {
		if filepath.IsAbs(x) {
			return filepath.Join(x, "sigil-copilot", "config.env")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return filepath.Join(home, ".config", "sigil-copilot", "config.env")
	}
	return filepath.Join(os.TempDir(), "sigil-copilot", "config.env")
}

func ApplyEnv(logger *log.Logger) map[string]string {
	fileEnv := LoadDotenv(FilePath(), logger)
	for k, v := range fileEnv {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			continue
		}
		_ = os.Setenv(k, v)
	}
	return fileEnv
}

func Load(logger *log.Logger) Config {
	return Config{
		ContentCapture: resolveContentCapture(os.Getenv("SIGIL_CONTENT_CAPTURE_MODE"), logger),
		Debug:          util.ParseBool(os.Getenv("SIGIL_DEBUG")),
	}
}

func resolveContentCapture(raw string, logger *log.Logger) sigil.ContentCaptureMode {
	if strings.TrimSpace(raw) == "" {
		return sigil.ContentCaptureModeMetadataOnly
	}
	var mode sigil.ContentCaptureMode
	if err := mode.UnmarshalText([]byte(raw)); err != nil {
		if logger != nil {
			logger.Printf("config: unknown SIGIL_CONTENT_CAPTURE_MODE=%q; using metadata_only", raw)
		}
		return sigil.ContentCaptureModeMetadataOnly
	}
	if mode == sigil.ContentCaptureModeDefault {
		return sigil.ContentCaptureModeMetadataOnly
	}
	return mode
}

func parseDotenvValue(v string) string {
	if len(v) >= 2 {
		first := v[0]
		if first == '"' || first == '\'' {
			if end := strings.IndexByte(v[1:], first); end >= 0 {
				return v[1 : 1+end]
			}
		}
	}
	if hashIdx := strings.Index(v, " #"); hashIdx >= 0 {
		v = strings.TrimRight(v[:hashIdx], " \t")
	}
	return v
}

func LoadDotenv(path string, logger *log.Logger) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) && logger != nil {
			logger.Printf("config: read %s: %v", path, err)
		}
		return out
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(line[len("export "):])
		}
		key, rest, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || !AllowedDotenvKey(key) {
			continue
		}
		value := parseDotenvValue(strings.TrimSpace(rest))
		if value != "" {
			out[key] = value
		}
	}
	if err := scanner.Err(); err != nil && logger != nil {
		logger.Printf("config: scan %s: %v", path, err)
	}
	return out
}

func AllowedDotenvKey(key string) bool {
	if strings.HasPrefix(key, "SIGIL_") {
		return true
	}
	switch key {
	case "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_SERVICE_NAME":
		return true
	default:
		return false
	}
}
