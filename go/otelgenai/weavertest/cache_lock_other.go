//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package weavertest

import "context"

type cacheLock struct{}

func acquireCacheLock(context.Context, string) (*cacheLock, error) {
	return &cacheLock{}, nil
}

func (*cacheLock) release() {}
