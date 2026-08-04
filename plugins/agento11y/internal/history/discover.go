package history

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"
)

// previewWorkers is how many files are previewed at once. Preview is bounded
// I/O against thousands of small files, where the per-file open dominates, so
// overlapping them cuts discovery several times over on a machine with a warm
// page cache. The cap keeps an import from saturating a laptop's disk queue.
func previewWorkers() int {
	return min(max(runtime.GOMAXPROCS(0), 1)*2, 16)
}

// DiscoverOptions tunes the shared discovery driver. The zero value is the
// production configuration.
type DiscoverOptions struct {
	// Now supplies the clock used for active-session detection. nil uses
	// time.Now.
	Now func() time.Time
	// ActiveWindow is how recently a file must have been written for its
	// session to count as in-progress. Zero uses defaultActiveWindow.
	ActiveWindow time.Duration
	// Roots overrides the importer's roots. Tests set it; production leaves it
	// empty so the importer decides.
	Roots []string
}

// Discover walks the importer's roots, previews every matching file, and
// returns the sessions sorted most-recent-first.
//
// It is the only discovery implementation. Each importer contributes
// [Importer.Roots], [Importer.Match], and [Importer.Preview]; walking,
// cancellation, warning collection, active-session detection, and ordering are
// shared, so a new importer for an agent with a Go live mapper adds no
// scaffolding.
func Discover(ctx context.Context, agent AgentID, imp Importer, opts DiscoverOptions) (Discovery, error) {
	if imp == nil {
		return Discovery{}, fmt.Errorf("history: no importer for %q", agent)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	window := opts.ActiveWindow
	if window == 0 {
		window = defaultActiveWindow
	}
	roots := opts.Roots
	if len(roots) == 0 {
		roots = imp.Roots()
	}

	d := Discovery{Agent: agent}
	at := now()
	var paths []string
	seen := map[string]bool{}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return d, err
		}
		files, err := walkFiles(ctx, root, imp.Match)
		if err != nil {
			if ctx.Err() != nil {
				return d, ctx.Err()
			}
			d.Warnings = append(d.Warnings, fmt.Sprintf("scan %s: %v", root, err))
		}
		for _, path := range files {
			if seen[path] {
				continue // overlapping roots must not double-import a session
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}

	sessions, warnings, err := previewAll(ctx, agent, imp, paths, at, window)
	d.Warnings = append(d.Warnings, warnings...)
	if err != nil {
		return d, err
	}
	d.Sessions = sessions

	// Most recent first, with the path as the tie-break so two sessions with
	// the same timestamp keep a stable order across runs.
	sort.SliceStable(d.Sessions, func(i, j int) bool {
		a, b := d.Sessions[i], d.Sessions[j]
		if !a.LastActivityAt.Equal(b.LastActivityAt) {
			return a.LastActivityAt.After(b.LastActivityAt)
		}
		return a.SourcePath < b.SourcePath
	})
	return d, nil
}

// previewAll previews paths concurrently and returns the usable sessions plus
// one warning per unreadable file. Results are gathered by index, so the order
// does not depend on which worker finished first.
func previewAll(ctx context.Context, agent AgentID, imp Importer, paths []string, at time.Time, window time.Duration) ([]SessionPreview, []string, error) {
	type slot struct {
		preview SessionPreview
		ok      bool
		warning string
	}
	slots := make([]slot, len(paths))

	var wg sync.WaitGroup
	next := make(chan int)
	wg.Go(func() {
		defer close(next)
		for i := range paths {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	})

	for range min(previewWorkers(), max(len(paths), 1)) {
		wg.Go(func() {
			for i := range next {
				if ctx.Err() != nil {
					return
				}
				path := paths[i]
				preview, ok, err := imp.Preview(ctx, path)
				if err != nil {
					if ctx.Err() == nil {
						slots[i] = slot{warning: fmt.Sprintf("read %s: %v", path, err)}
					}
					continue
				}
				if !ok {
					continue
				}
				preview.Agent = agent
				preview.SourcePath = path
				if info, statErr := os.Stat(path); statErr == nil {
					if preview.SizeBytes == 0 {
						preview.SizeBytes = info.Size()
					}
					if preview.LastActivityAt.IsZero() {
						preview.LastActivityAt = info.ModTime()
					}
					preview.Active = isActiveMod(info.ModTime(), at, window)
				}
				slots[i] = slot{preview: preview, ok: true}
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	sessions := make([]SessionPreview, 0, len(slots))
	var warnings []string
	for _, s := range slots {
		switch {
		case s.warning != "":
			warnings = append(warnings, s.warning)
		case s.ok:
			sessions = append(sessions, s.preview)
		}
	}
	return sessions, warnings, nil
}
