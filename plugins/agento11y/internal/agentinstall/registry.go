// Package agentinstall holds the noninteractive installer registry used by
// agento11y agents reconcile. Agent adapters register their own installer so
// the fleet command does not need a separately maintained list of agents.
package agentinstall

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
)

// InstallFunc configures one host integration without launching the host or
// prompting for credentials. changed is true only when it modified host state.
type InstallFunc func(ctx context.Context, stdout io.Writer, logger *log.Logger) (changed bool, err error)

// Spec describes one agent that agents reconcile can configure.
type Spec struct {
	Name          string
	Install       InstallFunc
	IsMissingHost func(error) bool
}

var (
	mu     sync.RWMutex
	byName = map[string]Spec{}
)

// Register adds one noninteractive installer. It is intended for adapter
// package init functions. Invalid or duplicate registrations are programmer
// errors, so they panic while the binary starts instead of silently changing
// a fleet command's behavior.
func Register(spec Spec) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		panic("agentinstall: register installer with empty name")
	}
	if strings.ContainsAny(spec.Name, ", \t\r\n") {
		panic(fmt.Sprintf("agentinstall: invalid installer name %q", spec.Name))
	}
	if spec.Install == nil {
		panic(fmt.Sprintf("agentinstall: register %q without an install function", spec.Name))
	}

	mu.Lock()
	defer mu.Unlock()
	if _, exists := byName[spec.Name]; exists {
		panic(fmt.Sprintf("agentinstall: duplicate installer registration for %q", spec.Name))
	}
	byName[spec.Name] = spec
}

// All returns the registered installers sorted by name. Each call returns an
// independent slice so callers cannot mutate the registry.
func All() []Spec {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Spec, 0, len(byName))
	for _, spec := range byName {
		out = append(out, spec)
	}
	slices.SortFunc(out, func(a, b Spec) int { return strings.Compare(a.Name, b.Name) })
	return out
}
