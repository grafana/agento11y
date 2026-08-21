package local

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// viewerEntry is the module esbuild starts from, relative to the source root.
// It holds the mount and nothing else, so every other module can be imported
// by a test without rendering the app.
const viewerEntry = "main.tsx"

// The two namespaces the viewer bundle is assembled from. Neither is
// esbuild's own file namespace: src reads the embedded sources (or the dev
// directory) through an fs.FS, and shim answers each bare import with a module
// esbuild never looks for on disk.
const (
	viewerSrcNamespace  = "agento11y-viewer-src"
	viewerShimNamespace = "agento11y-viewer-shim"
)

// viewerShims stands in for the packages the viewer imports. The daemon
// bundles with no node_modules, and the browser already has React, ReactDOM,
// and markdown-to-jsx as globals from the vendored scripts index.html loads.
// Each shim hands the bundle one of those globals under the package's normal
// name, so the source can `import { useState } from 'react'` the way any React
// project does, and @types/react and Testing Library work off the same
// imports.
//
// The three package shims assign the whole global to module.exports rather
// than listing names. esbuild resolves each named import through its CommonJS
// interop, so a hook the viewer starts using needs no edit here; an allowlist
// would fail the Go build after tsc and vitest had both passed against the
// real npm packages. react/jsx-runtime is the exception: React 18's UMD build
// carries no automatic runtime, so that shim synthesizes one.
//
// A bare import that is not listed here fails the build rather than resolving
// to nothing: the daemon has no package manager to fall back to.
var viewerShims = map[string]string{
	"react": `const React = globalThis.React;
if (!React) throw new Error('react: the vendored React script did not load');
module.exports = React;
`,
	"react-dom/client": `const ReactDOM = globalThis.ReactDOM;
if (!ReactDOM) throw new Error('react-dom/client: the vendored ReactDOM script did not load');
module.exports = ReactDOM;
`,
	// markdown-to-jsx is the one shim allowed to resolve to nothing. The
	// transcript is still readable as plain text without it, so ProseBlock
	// falls back to the raw text instead of the page throwing.
	"markdown-to-jsx": `module.exports = globalThis.MarkdownToJSX || {};
`,
	// The automatic JSX runtime, which the React 18 UMD build does not carry.
	// createElement reads key and ref out of the props object itself, so
	// folding the key argument back in is all jsx has to do. jsxs differs only
	// in promising that children is already an array, which createElement does
	// not care about.
	"react/jsx-runtime": `const React = globalThis.React;
if (!React) throw new Error('react/jsx-runtime: the vendored React script did not load');
export const Fragment = React.Fragment;
export function jsx(type, props, key) {
  return React.createElement(type, key === undefined ? props : { ...props, key });
}
export const jsxs = jsx;
`,
}

// viewerSourceExtensions is the order an extensionless import is resolved in.
var viewerSourceExtensions = []string{".tsx", ".ts", ".jsx", ".js"}

var (
	viewerBundleOnce sync.Once
	viewerBundleJS   []byte
	viewerBundleErr  error
)

// viewerBundle returns the compiled viewer as one IIFE.
//
// The embedded sources cannot change while the process runs, so that bundle is
// built once and kept. With AGENTO11Y_LOCAL_WEB_DIR (or the legacy
// SIGIL_LOCAL_WEB_DIR) set, every call rebuilds from $DIR/src so a browser
// reload picks up an edit. The dev path reads that directory and nothing else:
// falling back to the embedded copy per file, the way devAsset does, would
// resurrect a module deleted from the dev tree.
//
// A dev build that fails names the variable and the directory it read. Every
// way of pointing the variable somewhere wrong ends in the same esbuild
// message about a missing entry point, which on its own says nothing about
// where the daemon looked.
func viewerBundle() ([]byte, error) {
	if dir, key, ok := envconfig.LookupEnv("LOCAL_WEB_DIR"); ok {
		src := filepath.Join(dir, "src")
		bundle, err := buildViewerBundle(os.DirFS(src))
		if err != nil {
			return nil, fmt.Errorf("%s=%s: building the viewer from %s: %w", key, dir, src, err)
		}
		return bundle, nil
	}
	viewerBundleOnce.Do(func() {
		sources, err := fs.Sub(webSrc, "web/src")
		if err != nil {
			viewerBundleErr = err
			return
		}
		viewerBundleJS, viewerBundleErr = buildViewerBundle(sources)
	})
	return viewerBundleJS, viewerBundleErr
}

// buildViewerBundle compiles the viewer sources in sources into a single
// script. Errors carry esbuild's own messages, which name the file and line.
func buildViewerBundle(sources fs.FS) ([]byte, error) {
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{viewerEntry},
		Bundle:      true,
		Format:      api.FormatIIFE,
		Target:      api.ES2022,
		JSX:         api.JSXAutomatic,
		Plugins:     []api.Plugin{viewerPlugin(sources)},
		Write:       false,
		LogLevel:    api.LogLevelSilent,
	})
	if len(result.Errors) > 0 {
		messages := api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
		return nil, errors.New(strings.TrimSpace(strings.Join(messages, "")))
	}
	if len(result.OutputFiles) != 1 {
		return nil, fmt.Errorf("viewer bundle: esbuild produced %d output files, want 1", len(result.OutputFiles))
	}
	return result.OutputFiles[0].Contents, nil
}

// viewerPlugin teaches esbuild to read from sources instead of the filesystem,
// and to answer the bare imports with [viewerShims].
func viewerPlugin(sources fs.FS) api.Plugin {
	return api.Plugin{
		Name: "agento11y-viewer",
		Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: `.*`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				if _, ok := viewerShims[args.Path]; ok {
					return api.OnResolveResult{Path: args.Path, Namespace: viewerShimNamespace}, nil
				}
				if args.Kind == api.ResolveEntryPoint {
					return api.OnResolveResult{Path: args.Path, Namespace: viewerSrcNamespace}, nil
				}
				if !strings.HasPrefix(args.Path, "./") && !strings.HasPrefix(args.Path, "../") {
					return api.OnResolveResult{}, fmt.Errorf(
						"%q is imported by %q but is neither a relative path nor a shimmed package", args.Path, args.Importer)
				}
				resolved, err := resolveViewerSource(sources, path.Join(path.Dir(args.Importer), args.Path))
				if err != nil {
					return api.OnResolveResult{}, err
				}
				return api.OnResolveResult{Path: resolved, Namespace: viewerSrcNamespace}, nil
			})

			build.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: viewerShimNamespace},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					contents := viewerShims[args.Path]
					return api.OnLoadResult{Contents: &contents, Loader: api.LoaderJS}, nil
				})

			build.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: viewerSrcNamespace},
				func(args api.OnLoadArgs) (api.OnLoadResult, error) {
					body, err := fs.ReadFile(sources, args.Path)
					if err != nil {
						return api.OnLoadResult{}, err
					}
					contents := string(body)
					return api.OnLoadResult{Contents: &contents, Loader: viewerLoader(args.Path)}, nil
				})
		},
	}
}

// resolveViewerSource turns an import target into a path that exists in
// sources, trying the module extensions in turn when the import carries none.
func resolveViewerSource(sources fs.FS, target string) (string, error) {
	candidates := []string{target}
	if path.Ext(target) == "" {
		candidates = make([]string, 0, len(viewerSourceExtensions))
		for _, ext := range viewerSourceExtensions {
			candidates = append(candidates, target+ext)
		}
	}
	for _, candidate := range candidates {
		if _, err := fs.Stat(sources, candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no viewer module at %q", target)
}

func viewerLoader(name string) api.Loader {
	switch path.Ext(name) {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	default:
		return api.LoaderJS
	}
}
