// Package weavertest prepares the pinned OpenTelemetry GenAI
// semantic-convention registry and checks telemetry against it with
// [Weaver].
//
// The policies and the Weaver configuration are embedded, so a caller needs
// no files of its own. [Setup] writes them next to the cached registry and
// returns the paths. Two runners consume those assets: [Start] sends live
// telemetry over OTLP, and [LiveCheck] checks recorded spans from a file.
//
// [Weaver]: https://github.com/open-telemetry/weaver
package weavertest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// inputs holds the Rego policies and the Weaver configuration. They are
// embedded rather than read from the repository so that a consumer outside
// this repository gets the same rules as the SDK's own conformance suite.
//
//go:embed policies/*.rego weaver.toml
var inputs embed.FS

const (
	fetchTimeout          = 60 * time.Second
	registryStampContents = "v1\n"
)

// maxEntryBytes limits the size of each extracted file. The registry's largest
// file is a few hundred kilobytes, well below the 64 MiB default. Tests can
// lower the limit.
var maxEntryBytes int64 = 64 << 20

var (
	migratedDirs   = []string{"gen-ai", "mcp", "openai"}
	migratedGroups = []struct {
		file string
		id   string
	}{
		{file: "aws/registry.yaml", id: "registry.aws.bedrock"},
	}
)

// Assets are the local registry, policy and configuration inputs for Weaver.
type Assets struct {
	// Registry is the model directory of the prepared GenAI registry.
	Registry string
	// Policies is the directory holding the Rego advice policies.
	Policies string
	// Config is the Weaver configuration file.
	Config string
	// AdviceData is a glob of the registry's JSON schema advice data.
	AdviceData string
	// RegistryRef is the semantic-conventions-genai commit the registry
	// was built from.
	RegistryRef string
	// UpstreamVersion is the semantic-conventions release that commit
	// depends on, read from the registry's versions.env.
	UpstreamVersion string
}

// Setup prepares the GenAI registry at registryRef and returns Weaver's local
// inputs. registryRef must identify a commit in
// open-telemetry/semantic-conventions-genai. The registry has no tagged
// releases. A cold run downloads the selected commit and the upstream
// semantic-conventions release pinned by that commit. Later runs reuse
// $SEMCONV_CACHE, or the user cache directory if the variable is unset.
func Setup(ctx context.Context, registryRef string) (Assets, error) {
	if registryRef == "" {
		return Assets{}, errors.New("registry ref is empty")
	}

	registryRoot, err := provisionRegistry(ctx, registryRef)
	if err != nil {
		return Assets{}, err
	}
	upstream, err := upstreamVersion(registryRoot)
	if err != nil {
		return Assets{}, err
	}
	policies, config, err := writeInputs(registryRoot)
	if err != nil {
		return Assets{}, err
	}

	return Assets{
		Registry:        filepath.Join(registryRoot, "model"),
		Policies:        policies,
		Config:          config,
		AdviceData:      adviceDataGlob(registryRoot),
		RegistryRef:     registryRef,
		UpstreamVersion: upstream,
	}, nil
}

type embeddedInput struct {
	name     string
	contents []byte
}

// writeInputs installs the embedded policies and configuration below the
// prepared registry and returns the policy directory and the config file. Each
// content digest has its own directory, and writeInputs never replaces it. A
// rename publishes all files together for concurrent callers.
func writeInputs(registryRoot string) (policies string, config string, err error) {
	files, digest, err := loadEmbeddedInputs()
	if err != nil {
		return "", "", err
	}
	root := filepath.Join(registryRoot, ".weavertest-"+digest)
	policies = filepath.Join(root, "policies")
	config = filepath.Join(root, "weaver.toml")
	if isDir(root) {
		return policies, config, nil
	}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", fmt.Errorf("create Weaver input cache: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".weavertest-staging-")
	if err != nil {
		return "", "", fmt.Errorf("create Weaver input staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	for _, file := range files {
		target := filepath.Join(staging, filepath.FromSlash(file.name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", "", fmt.Errorf("create Weaver input directory: %w", err)
		}
		if err := os.WriteFile(target, file.contents, 0o644); err != nil {
			return "", "", fmt.Errorf("write %s: %w", target, err)
		}
	}
	if err := os.Rename(staging, root); err != nil && !isDir(root) {
		return "", "", fmt.Errorf("install Weaver inputs: %w", err)
	}
	return policies, config, nil
}

func loadEmbeddedInputs() ([]embeddedInput, string, error) {
	entries, err := fs.ReadDir(inputs, "policies")
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		names = append(names, "policies/"+entry.Name())
	}
	names = append(names, "weaver.toml")

	files := make([]embeddedInput, 0, len(names))
	var digestInput []byte
	for _, name := range names {
		contents, err := inputs.ReadFile(name)
		if err != nil {
			return nil, "", err
		}
		files = append(files, embeddedInput{name: name, contents: contents})
		digestInput = append(digestInput, name...)
		digestInput = append(digestInput, 0)
		digestInput = binary.AppendUvarint(digestInput, uint64(len(contents)))
		digestInput = append(digestInput, contents...)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(digestInput))
	return files, digest, nil
}

// upstreamVersion reads the semantic-conventions release the GenAI registry
// depends on.
func upstreamVersion(registryRoot string) (string, error) {
	pins, err := loadPins(filepath.Join(registryRoot, "versions.env"))
	if err != nil {
		return "", err
	}
	version := pins["SEMCONV_VERSION"]
	if version == "" {
		return "", errors.New("GenAI registry versions.env has no SEMCONV_VERSION")
	}
	return version, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func loadPins(path string) (map[string]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read version pins: %w", err)
	}
	pins := make(map[string]string)
	for raw := range strings.SplitSeq(string(contents), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid version pin %q in %s", raw, path)
		}
		pins[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
	}
	return pins, nil
}

func cacheDir() (string, error) {
	if override := os.Getenv("SEMCONV_CACHE"); override != "" {
		return override, nil
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(root, "otel-conformance", "semconv"), nil
}

func provisionRegistry(ctx context.Context, genAIRef string) (string, error) {
	cache, err := cacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", fmt.Errorf("create semantic-convention cache: %w", err)
	}
	lock, err := acquireCacheLock(ctx, filepath.Join(cache, ".provision.lock"))
	if err != nil {
		return "", err
	}
	defer lock.release()

	genAIRoot := filepath.Join(cache, "genai-"+genAIRef)
	if isProvisioned(genAIRoot) {
		return genAIRoot, nil
	}

	genAIURL := "https://github.com/open-telemetry/semantic-conventions-genai/archive/" + genAIRef + ".tar.gz"
	if err := downloadAndExtract(ctx, genAIURL, genAIRoot, "genai-semconv"); err != nil {
		return "", err
	}
	upstream, err := upstreamVersion(genAIRoot)
	if err != nil {
		return "", err
	}

	upstreamRoot := filepath.Join(cache, "upstream-"+upstream)
	if !isDir(filepath.Join(upstreamRoot, "model")) {
		upstreamURL := "https://github.com/open-telemetry/semantic-conventions/archive/refs/tags/" + upstream + ".tar.gz"
		if err := downloadAndExtract(ctx, upstreamURL, upstreamRoot, "upstream-semconv"); err != nil {
			return "", err
		}
	}
	filtered, err := materializeFilteredUpstream(genAIRoot, upstreamRoot)
	if err != nil {
		return "", err
	}
	if err := rewriteManifestDependency(genAIRoot, filtered); err != nil {
		return "", err
	}
	if _, err := patchAdviceData(genAIRoot); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(genAIRoot, ".provisioned"), []byte(registryStampContents), 0o644); err != nil {
		return "", fmt.Errorf("write registry provision stamp: %w", err)
	}
	return genAIRoot, nil
}

func isProvisioned(genAIRoot string) bool {
	contents, err := os.ReadFile(filepath.Join(genAIRoot, ".provisioned"))
	return err == nil && string(contents) == registryStampContents
}

func downloadAndExtract(ctx context.Context, url, target, label string) error {
	client := &http.Client{Timeout: fetchTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build %s request: %w", label, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fetch %s: HTTP %s", label, resp.Status)
	}
	compressed, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("open %s archive: %w", label, err)
	}
	defer compressed.Close()

	tmp, err := os.MkdirTemp(filepath.Dir(target), label+"-")
	if err != nil {
		return fmt.Errorf("create %s extraction directory: %w", label, err)
	}
	defer os.RemoveAll(tmp)
	extractRoot := filepath.Join(tmp, "extract")
	if err := os.Mkdir(extractRoot, 0o755); err != nil {
		return err
	}
	if err := extractTar(compressed, extractRoot); err != nil {
		return fmt.Errorf("extract %s: %w", label, err)
	}
	entries, err := os.ReadDir(extractRoot)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return fmt.Errorf("unexpected %s archive layout", label)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("replace %s cache: %w", label, err)
	}
	if err := os.Rename(filepath.Join(extractRoot, entries[0].Name()), target); err != nil {
		return fmt.Errorf("install %s cache: %w", label, err)
	}
	return nil
}

func extractTar(src io.Reader, target string) error {
	reader := tar.NewReader(src)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry escapes extraction root: %q", header.Name)
		}
		if err := checkNoSymlinkParent(target, name); err != nil {
			return err
		}
		path := filepath.Join(target, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, fs.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			written, copyErr := io.Copy(file, io.LimitReader(reader, maxEntryBytes+1))
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			if written > maxEntryBytes {
				return fmt.Errorf("archive entry %q is larger than %d bytes", header.Name, maxEntryBytes)
			}
		case tar.TypeSymlink:
			linkTarget := filepath.Clean(filepath.Join(filepath.Dir(name), filepath.FromSlash(header.Linkname)))
			if filepath.IsAbs(header.Linkname) || linkTarget == ".." || strings.HasPrefix(linkTarget, ".."+string(filepath.Separator)) {
				return fmt.Errorf("archive symlink escapes extraction root: %q -> %q", header.Name, header.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(header.Linkname), path); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		default:
			return fmt.Errorf("unsupported archive entry type %d for %q", header.Typeflag, header.Name)
		}
	}
}

// checkNoSymlinkParent rejects paths that traverse symlinks from earlier
// archive entries. Lexical checks cannot detect a symlink chain whose
// individual targets stay inside the extraction root but resolve outside it
// when combined.
func checkNoSymlinkParent(target, name string) error {
	current := target
	for part := range strings.SplitSeq(name, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing below this component exists yet either.
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %q writes through symlink %q", name, current)
		}
	}
	return nil
}

func materializeFilteredUpstream(genAIRoot, upstreamRoot string) (string, error) {
	filtered := filepath.Join(genAIRoot, ".build", "sc-upstream-filtered")
	if err := os.RemoveAll(filtered); err != nil {
		return "", fmt.Errorf("clear filtered upstream registry: %w", err)
	}
	if err := copyDir(filepath.Join(upstreamRoot, "model"), filtered); err != nil {
		return "", fmt.Errorf("copy upstream registry: %w", err)
	}
	for _, migrated := range migratedDirs {
		if err := os.RemoveAll(filepath.Join(filtered, migrated)); err != nil {
			return "", fmt.Errorf("remove migrated registry directory %s: %w", migrated, err)
		}
	}
	for _, migrated := range migratedGroups {
		path := filepath.Join(filtered, filepath.FromSlash(migrated.file))
		contents, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		stripped := stripGroupBlock(string(contents), migrated.id)
		if err := os.WriteFile(path, []byte(stripped), 0o644); err != nil {
			return "", err
		}
	}
	return filtered, nil
}

func copyDir(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file %s with mode %s", path, info.Mode())
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		sourceFile, err := os.Open(path)
		if err != nil {
			return err
		}
		destinationFile, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = sourceFile.Close()
			return err
		}
		_, copyErr := io.Copy(destinationFile, sourceFile)
		return errors.Join(copyErr, sourceFile.Close(), destinationFile.Close())
	})
}

func stripGroupBlock(contents, groupID string) string {
	var out strings.Builder
	skip := false
	target := "  - id: " + groupID
	for chunk := range strings.SplitAfterSeq(contents, "\n") {
		line := strings.TrimSuffix(chunk, "\n")
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "  - id: ") {
			skip = line == target
		}
		if !skip {
			out.WriteString(chunk)
		}
	}
	return out.String()
}

func rewriteManifestDependency(genAIRoot, filtered string) error {
	manifest := filepath.Join(genAIRoot, "model", "manifest.yaml")
	contents, err := os.ReadFile(manifest)
	if err != nil {
		return fmt.Errorf("read GenAI manifest: %w", err)
	}
	pattern := regexp.MustCompile(`(?m)^(\s*registry_path:\s*)\./\.build/sc-upstream-filtered\s*$`)
	matches := pattern.FindAllIndex(contents, -1)
	if len(matches) != 1 {
		return fmt.Errorf("expected one filtered registry path in %s, found %d", manifest, len(matches))
	}
	absolute, err := filepath.Abs(filtered)
	if err != nil {
		return err
	}
	replacement := []byte("${1}" + filepath.ToSlash(absolute))
	updated := pattern.ReplaceAll(contents, replacement)
	if err := os.WriteFile(manifest, updated, 0o644); err != nil {
		return fmt.Errorf("rewrite GenAI manifest: %w", err)
	}
	return nil
}

func adviceDataGlob(genAIRoot string) string {
	return filepath.ToSlash(filepath.Join(genAIRoot, "model", "gen-ai", "*.json"))
}

func patchAdviceData(genAIRoot string) (string, error) {
	dir := filepath.Join(genAIRoot, "model", "gen-ai")
	toolSchema := filepath.Join(dir, "gen-ai-tool-definitions.json")
	contents, err := os.ReadFile(toolSchema)
	if err != nil {
		return "", fmt.Errorf("read tool-definition schema: %w", err)
	}
	// Weaver's Rego engine does not fetch this external meta-schema. Retain
	// the object check while leaving the schema contents outside this suite's
	// validation scope.
	updated := strings.ReplaceAll(
		string(contents),
		`"$ref": "http://json-schema.org/draft-07/schema#"`,
		`"type": "object"`,
	)
	if updated != string(contents) {
		if err := os.WriteFile(toolSchema, []byte(updated), 0o644); err != nil {
			return "", fmt.Errorf("patch tool-definition schema: %w", err)
		}
	}
	return adviceDataGlob(genAIRoot), nil
}
