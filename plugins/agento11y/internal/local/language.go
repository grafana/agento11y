package local

import (
	"encoding/base64"
	"encoding/json"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	languageSharedID   = "shared"
	languageSharedName = "Shared / unlinked"

	// maxLanguagePathsPerGeneration caps how many distinct paths one
	// generation can contribute. The summary cache keeps these paths for
	// the life of the file, so a huge directory listing must not land there.
	maxLanguagePathsPerGeneration = 500
)

// LanguageAggregate is one language in the analytics mix. Token buckets are
// the portion of each session's usage allocated to that language, not the
// session total. Sessions counts conversations that touched at least one
// distinct file of this language, so shares across rows can sum past 100%.
type LanguageAggregate struct {
	ID                  string                  `json:"id"`
	Name                string                  `json:"name"`
	Sessions            int                     `json:"sessions"`
	Files               int                     `json:"files"`
	TokenBuckets        TokenBuckets            `json:"token_buckets"`
	TokenBucketsByModel map[string]TokenBuckets `json:"token_buckets_by_model"`
}

// languageTouch is one generation's distinct normalized file paths.
// Timestamp is generationTime, the same clock period metrics clip on.
type languageTouch struct {
	Timestamp time.Time
	Paths     []string
}

var toolPathKeys = map[string]struct{}{
	"file_path":       {},
	"filepath":        {},
	"filePath":        {},
	"path":            {},
	"target_file":     {},
	"targetFile":      {},
	"notebook_path":   {},
	"notebookPath":    {},
	"target_notebook": {},
	"targetNotebook":  {},
}

var patchFileMarkers = []string{
	"*** Update File:",
	"*** Add File:",
	"*** Delete File:",
	"*** Move to:",
}

// extensionLanguages maps a lower-case extension, without the dot, to a
// stable language id. Configuration and documentation are first-class
// buckets: a coding session's JSON and Markdown are part of the mix, not
// leftovers of the language the agent was editing.
var extensionLanguages = map[string]string{
	"go":            "go",
	"ts":            "typescript",
	"tsx":           "typescript",
	"mts":           "typescript",
	"cts":           "typescript",
	"js":            "javascript",
	"jsx":           "javascript",
	"mjs":           "javascript",
	"cjs":           "javascript",
	"py":            "python",
	"pyi":           "python",
	"pyw":           "python",
	"java":          "java",
	"kt":            "kotlin",
	"kts":           "kotlin",
	"scala":         "scala",
	"rs":            "rust",
	"rb":            "ruby",
	"php":           "php",
	"c":             "c",
	"h":             "c",
	"cc":            "cpp",
	"cpp":           "cpp",
	"cxx":           "cpp",
	"hh":            "cpp",
	"hpp":           "cpp",
	"hxx":           "cpp",
	"cs":            "csharp",
	"swift":         "swift",
	"sh":            "shell",
	"bash":          "shell",
	"zsh":           "shell",
	"fish":          "shell",
	"ksh":           "shell",
	"ps1":           "shell",
	"psm1":          "shell",
	"bat":           "shell",
	"cmd":           "shell",
	"css":           "css",
	"scss":          "css",
	"sass":          "css",
	"less":          "css",
	"html":          "html",
	"htm":           "html",
	"sql":           "sql",
	"proto":         "protobuf",
	"graphql":       "graphql",
	"gql":           "graphql",
	"vue":           "vue",
	"svelte":        "svelte",
	"lua":           "lua",
	"pl":            "perl",
	"pm":            "perl",
	"ex":            "elixir",
	"exs":           "elixir",
	"clj":           "clojure",
	"cljs":          "clojure",
	"cljc":          "clojure",
	"hs":            "haskell",
	"dart":          "dart",
	"zig":           "zig",
	"r":             "r",
	"jl":            "julia",
	"elm":           "elm",
	"erl":           "erlang",
	"fs":            "fsharp",
	"fsx":           "fsharp",
	"vb":            "vb",
	"m":             "objectivec",
	"mm":            "objectivec",
	"md":            "documentation",
	"mdx":           "documentation",
	"markdown":      "documentation",
	"rst":           "documentation",
	"adoc":          "documentation",
	"asciidoc":      "documentation",
	"txt":           "documentation",
	"text":          "documentation",
	"json":          "configuration",
	"jsonc":         "configuration",
	"json5":         "configuration",
	"yaml":          "configuration",
	"yml":           "configuration",
	"toml":          "configuration",
	"ini":           "configuration",
	"cfg":           "configuration",
	"conf":          "configuration",
	"config":        "configuration",
	"properties":    "configuration",
	"xml":           "configuration",
	"plist":         "configuration",
	"env":           "configuration",
	"lock":          "configuration",
	"hcl":           "configuration",
	"tf":            "configuration",
	"tfvars":        "configuration",
	"gradle":        "configuration",
	"csproj":        "configuration",
	"sln":           "configuration",
	"cabal":         "configuration",
	"nix":           "configuration",
	"editorconfig":  "configuration",
	"gitignore":     "configuration",
	"gitattributes": "configuration",
	"npmrc":         "configuration",
	"nvmrc":         "configuration",
	"dockerignore":  "configuration",
}

// basenameLanguages wins over the extension. requirements.txt is a
// dependency manifest, not prose, and Dockerfile has no extension at all.
var basenameLanguages = map[string]string{
	"dockerfile":       "configuration",
	"containerfile":    "configuration",
	"makefile":         "configuration",
	"gnumakefile":      "configuration",
	"justfile":         "configuration",
	"jenkinsfile":      "configuration",
	"procfile":         "configuration",
	"gemfile":          "configuration",
	"rakefile":         "configuration",
	"podfile":          "configuration",
	"brewfile":         "configuration",
	"vagrantfile":      "configuration",
	"go.mod":           "configuration",
	"go.sum":           "configuration",
	"requirements.txt": "configuration",
	"pipfile":          "configuration",
	"pipfile.lock":     "configuration",
	"cmakelists.txt":   "configuration",
	"readme":           "documentation",
	"license":          "documentation",
	"licence":          "documentation",
	"changelog":        "documentation",
	"copying":          "documentation",
	"authors":          "documentation",
	"contributors":     "documentation",
	"notice":           "documentation",
	"todo":             "documentation",
}

var languageDisplayNames = map[string]string{
	"c":             "C",
	"clojure":       "Clojure",
	"configuration": "Configuration",
	"cpp":           "C++",
	"csharp":        "C#",
	"css":           "CSS",
	"dart":          "Dart",
	"documentation": "Documentation",
	"elixir":        "Elixir",
	"elm":           "Elm",
	"erlang":        "Erlang",
	"fsharp":        "F#",
	"go":            "Go",
	"graphql":       "GraphQL",
	"haskell":       "Haskell",
	"html":          "HTML",
	"java":          "Java",
	"javascript":    "JavaScript",
	"julia":         "Julia",
	"kotlin":        "Kotlin",
	"lua":           "Lua",
	"objectivec":    "Objective-C",
	"perl":          "Perl",
	"php":           "PHP",
	"protobuf":      "Protobuf",
	"python":        "Python",
	"r":             "R",
	"ruby":          "Ruby",
	"rust":          "Rust",
	"scala":         "Scala",
	"shell":         "Shell",
	"sql":           "SQL",
	"svelte":        "Svelte",
	"swift":         "Swift",
	"typescript":    "TypeScript",
	"vb":            "Visual Basic",
	"vue":           "Vue",
	"zig":           "Zig",
}

func languageDisplayName(id string) string {
	if name, ok := languageDisplayNames[id]; ok {
		return name
	}
	if id == "" {
		return ""
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

func isPatchTool(name string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(name)), "patch")
}

// filePathsFromToolInput pulls file paths out of one tool call without
// keeping the call's contents. Path-shaped keys contribute their string
// values. Patch tools also contribute Codex-style "*** Update File:" lines.
func filePathsFromToolInput(tool string, raw json.RawMessage) []string {
	decoded := unwrapToolInput(raw)
	if len(decoded) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(decoded, &value); err != nil {
		if isPatchTool(tool) {
			return limitPaths(patchFilePaths(string(decoded)))
		}
		return nil
	}
	walker := pathWalker{seen: map[string]struct{}{}, patch: isPatchTool(tool)}
	walker.walk(value)
	return walker.paths
}

func unwrapToolInput(raw json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	raw = json.RawMessage(trimmed)
	if raw[0] != '"' {
		return append([]byte(nil), raw...)
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil && json.Valid(decoded) {
		return decoded
	}
	if json.Valid([]byte(encoded)) {
		return []byte(encoded)
	}
	quoted, err := json.Marshal(encoded)
	if err != nil {
		return nil
	}
	return quoted
}

type pathWalker struct {
	paths []string
	seen  map[string]struct{}
	patch bool
}

func (w *pathWalker) walk(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if _, ok := toolPathKeys[key]; ok {
				w.takePath(child)
				continue
			}
			w.walk(child)
		}
	case []any:
		for _, child := range typed {
			w.walk(child)
		}
	case string:
		if w.patch {
			for _, found := range patchFilePaths(typed) {
				w.addNormalized(found)
			}
		}
	}
}

func (w *pathWalker) takePath(value any) {
	switch typed := value.(type) {
	case string:
		w.addPath(typed)
	case []any:
		for _, child := range typed {
			if text, ok := child.(string); ok {
				w.addPath(text)
				continue
			}
			w.walk(child)
		}
	default:
		w.walk(typed)
	}
}

func (w *pathWalker) addPath(raw string) {
	w.addNormalized(normalizeToolPath(raw))
}

func (w *pathWalker) addNormalized(normalized string) {
	if normalized == "" || len(w.paths) >= maxLanguagePathsPerGeneration {
		return
	}
	if _, ok := w.seen[normalized]; ok {
		return
	}
	w.seen[normalized] = struct{}{}
	w.paths = append(w.paths, normalized)
}

func limitPaths(paths []string) []string {
	if len(paths) <= maxLanguagePathsPerGeneration {
		return paths
	}
	return paths[:maxLanguagePathsPerGeneration]
}

func patchFilePaths(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range patchFileMarkers {
			rest, ok := strings.CutPrefix(line, marker)
			if !ok {
				continue
			}
			if normalized := normalizeToolPath(rest); normalized != "" {
				out = append(out, normalized)
			}
			break
		}
	}
	return out
}

// normalizeToolPath returns a stable path for classification, or "" when
// the value is a directory, a glob, a URL, or otherwise not a file.
// Grep and Glob pass the search root in path without a trailing slash, so
// a path is a directory unless its base has an extension or is a known
// extensionless filename such as Dockerfile.
func normalizeToolPath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 1024 || strings.ContainsAny(trimmed, "*?\r\n") {
		return ""
	}
	if strings.Contains(trimmed, "://") {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "\\", "/")
	if strings.HasSuffix(trimmed, "/") {
		return ""
	}
	cleaned := path.Clean(trimmed)
	switch cleaned {
	case ".", "..", "/":
		return ""
	}
	if !looksLikeFile(cleaned) {
		return ""
	}
	return cleaned
}

func looksLikeFile(cleaned string) bool {
	base := strings.ToLower(path.Base(cleaned))
	if strings.HasPrefix(base, ".env") {
		return true
	}
	if _, ok := basenameLanguages[base]; ok {
		return true
	}
	return path.Ext(base) != ""
}

// classifyToolPath reports the language id for a normalized path, or ""
// when the file does not belong to a known language or file class.
func classifyToolPath(filePath string) string {
	base := strings.ToLower(path.Base(filePath))
	if strings.HasPrefix(base, ".env") {
		return "configuration"
	}
	if id, ok := basenameLanguages[base]; ok {
		return id
	}
	ext := strings.TrimPrefix(path.Ext(base), ".")
	if ext == "" {
		return ""
	}
	return extensionLanguages[ext]
}

func generationToolPaths(gen summaryGeneration) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(paths []string) {
		for _, filePath := range paths {
			if len(out) >= maxLanguagePathsPerGeneration {
				return
			}
			if _, ok := seen[filePath]; ok {
				continue
			}
			seen[filePath] = struct{}{}
			out = append(out, filePath)
		}
	}
	for _, message := range gen.ToolInput {
		for _, part := range message.Parts {
			if part.ToolCall != nil {
				add(part.ToolCall.Paths)
			}
		}
	}
	for _, message := range gen.ToolOutput {
		for _, part := range message.Parts {
			if part.ToolCall != nil {
				add(part.ToolCall.Paths)
			}
		}
	}
	return out
}

// countLanguageFiles dedupes paths across the in-period touches. The map
// counts recognized languages. The int counts paths that normalized but
// did not classify; those still take a share of the session.
func countLanguageFiles(touches []languageTouch, since, before time.Time) (map[string]int, int) {
	seen := map[string]struct{}{}
	counts := map[string]int{}
	var unclassified int
	for _, touch := range touches {
		if !inPeriod(touch.Timestamp, since, before) {
			continue
		}
		for _, filePath := range touch.Paths {
			if _, ok := seen[filePath]; ok {
				continue
			}
			seen[filePath] = struct{}{}
			id := classifyToolPath(filePath)
			if id == "" {
				unclassified++
				continue
			}
			counts[id]++
		}
	}
	if len(counts) == 0 {
		return nil, unclassified
	}
	return counts, unclassified
}

type languageAccumulator struct {
	sessions int
	files    int
	buckets  TokenBuckets
	byModel  map[string]TokenBuckets
}

func (a *languageAccumulator) add(buckets TokenBuckets, byModel map[string]TokenBuckets) {
	a.buckets = a.buckets.plus(buckets)
	if a.byModel == nil {
		a.byModel = map[string]TokenBuckets{}
	}
	for model, part := range byModel {
		a.byModel[model] = a.byModel[model].plus(part)
	}
}

func (a *languageAccumulator) aggregate(id, name string) LanguageAggregate {
	byModel := a.byModel
	if byModel == nil {
		byModel = map[string]TokenBuckets{}
	}
	return LanguageAggregate{
		ID:                  id,
		Name:                name,
		Sessions:            a.sessions,
		Files:               a.files,
		TokenBuckets:        a.buckets,
		TokenBucketsByModel: byModel,
	}
}

// aggregateLanguageMix splits every session's token buckets across the
// distinct files that session touched. A session with no file path, and
// the unrecognized-file share of every other session, lands on shared.
// language_sessions is the count of sessions in rows, the session-share
// denominator.
func aggregateLanguageMix(rows []ConversationSummary) ([]LanguageAggregate, LanguageAggregate, int) {
	recognized := map[string]*languageAccumulator{}
	shared := &languageAccumulator{byModel: map[string]TokenBuckets{}}
	for _, row := range rows {
		ids, weights := orderedLanguageWeights(row)
		byModel := row.TokenBucketsByModel
		if len(byModel) == 0 {
			byModel = map[string]TokenBuckets{"": row.TokenBuckets}
		}
		if len(ids) == 0 {
			shared.sessions++
			shared.add(sumModelBuckets(byModel), byModel)
			continue
		}
		allocated := map[string]map[string]TokenBuckets{}
		totals := map[string]TokenBuckets{}
		for model, buckets := range byModel {
			parts := splitBuckets(buckets, weights)
			for i, id := range ids {
				if allocated[id] == nil {
					allocated[id] = map[string]TokenBuckets{}
				}
				allocated[id][model] = parts[i]
				totals[id] = totals[id].plus(parts[i])
			}
		}
		for _, id := range ids {
			target := shared
			if id != "" {
				target = recognized[id]
				if target == nil {
					target = &languageAccumulator{byModel: map[string]TokenBuckets{}}
					recognized[id] = target
				}
				target.sessions++
				target.files += row.languageFiles[id]
			} else {
				target.sessions++
				target.files += row.unclassifiedFiles
			}
			target.add(totals[id], allocated[id])
		}
	}
	out := make([]LanguageAggregate, 0, len(recognized))
	for id, acc := range recognized {
		out = append(out, acc.aggregate(id, languageDisplayName(id)))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].TokenBuckets.total() != out[j].TokenBuckets.total() {
			return out[i].TokenBuckets.total() > out[j].TokenBuckets.total()
		}
		return out[i].Name < out[j].Name
	})
	return out, shared.aggregate(languageSharedID, languageSharedName), len(rows)
}

func orderedLanguageWeights(row ConversationSummary) ([]string, []int) {
	if len(row.languageFiles) == 0 && row.unclassifiedFiles == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(row.languageFiles)+1)
	for id, count := range row.languageFiles {
		if id != "" && count > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if row.unclassifiedFiles > 0 {
		ids = append(ids, "")
	}
	weights := make([]int, len(ids))
	for i, id := range ids {
		if id == "" {
			weights[i] = row.unclassifiedFiles
			continue
		}
		weights[i] = row.languageFiles[id]
	}
	return ids, weights
}

func sumModelBuckets(byModel map[string]TokenBuckets) TokenBuckets {
	var total TokenBuckets
	for _, buckets := range byModel {
		total = total.plus(buckets)
	}
	return total
}

func splitBuckets(total TokenBuckets, weights []int) []TokenBuckets {
	fresh := splitInt64(total.FreshInput, weights)
	cacheRead := splitInt64(total.CacheRead, weights)
	cacheWrite := splitInt64(total.CacheWrite, weights)
	output := splitInt64(total.Output, weights)
	reasoning := splitInt64(total.Reasoning, weights)
	out := make([]TokenBuckets, len(weights))
	for i := range out {
		out[i] = TokenBuckets{
			FreshInput: fresh[i],
			CacheRead:  cacheRead[i],
			CacheWrite: cacheWrite[i],
			Output:     output[i],
			Reasoning:  reasoning[i],
		}
	}
	return out
}

// splitInt64 divides total across weights using the largest-remainder
// method, so the parts sum back to total. Equal remainders prefer the
// earlier weight.
func splitInt64(total int64, weights []int) []int64 {
	out := make([]int64, len(weights))
	if total <= 0 || len(weights) == 0 {
		return out
	}
	var denom int64
	for _, weight := range weights {
		if weight > 0 {
			denom += int64(weight)
		}
	}
	if denom == 0 {
		return out
	}
	type remainder struct {
		index int
		frac  int64
	}
	remainders := make([]remainder, 0, len(weights))
	var used int64
	for i, weight := range weights {
		if weight <= 0 {
			continue
		}
		num := total * int64(weight)
		out[i] = num / denom
		used += out[i]
		remainders = append(remainders, remainder{index: i, frac: num % denom})
	}
	leftover := total - used
	sort.Slice(remainders, func(i, j int) bool {
		if remainders[i].frac != remainders[j].frac {
			return remainders[i].frac > remainders[j].frac
		}
		return remainders[i].index < remainders[j].index
	})
	for i := int64(0); i < leftover && int(i) < len(remainders); i++ {
		out[remainders[i].index]++
	}
	return out
}
