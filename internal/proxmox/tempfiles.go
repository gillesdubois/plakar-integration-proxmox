/*
 * Copyright (c) 2026 Gilles DUBOIS
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package proxmox

import (
	"context"
	"io"
	"sync"
)

// TempFiles tracks the artefacts an operation leaves in dump_dir so they are
// removed on every exit path, failures included.
type TempFiles struct {
	client  *Client
	enabled bool

	mu    sync.Mutex
	paths map[string]struct{}
}

func NewTempFiles(client *Client, enabled bool) *TempFiles {
	return &TempFiles{
		client:  client,
		enabled: enabled,
		paths:   make(map[string]struct{}),
	}
}

// Track registers a file for removal by Sweep.
func (t *TempFiles) Track(filepath string) {
	if filepath == "" {
		return
	}

	t.mu.Lock()
	t.paths[filepath] = struct{}{}
	t.mu.Unlock()
}

// Forget drops a file from the tracking set without removing it.
func (t *TempFiles) Forget(filepath string) {
	t.mu.Lock()
	delete(t.paths, filepath)
	t.mu.Unlock()
}

// Remove deletes a tracked file now, along with the vzdump log sitting beside
// it, and stops tracking it.
func (t *TempFiles) Remove(ctx context.Context, filepath string) error {
	t.Forget(filepath)
	if !t.enabled || filepath == "" {
		return nil
	}

	if err := t.client.Remove(ctx, filepath); err != nil {
		return err
	}

	if logPath := DumpLogPath(filepath); logPath != "" {
		_ = t.client.Remove(ctx, logPath)
	}
	return nil
}

// Adopt hands a tracked file over to a reader: the file lives until that reader
// is closed, then is removed.
func (t *TempFiles) Adopt(ctx context.Context, filepath string, reader io.ReadCloser) io.ReadCloser {
	t.Forget(filepath)
	if !t.enabled {
		return reader
	}

	cleanupCtx := context.WithoutCancel(ctx)

	return &cleanupReadCloser{
		ReadCloser: reader,
		cleanup: func() {
			if err := t.Remove(cleanupCtx, filepath); err != nil {
				Warnf("unable to remove temporary dump %s: %v", filepath, err)
			}
		},
	}
}

// Sweep removes every file still tracked, best effort.
func (t *TempFiles) Sweep(ctx context.Context) {
	t.mu.Lock()
	paths := make([]string, 0, len(t.paths))
	for filepath := range t.paths {
		paths = append(paths, filepath)
	}
	t.paths = make(map[string]struct{})
	t.mu.Unlock()

	if !t.enabled {
		return
	}

	cleanupCtx := context.WithoutCancel(ctx)
	for _, filepath := range paths {
		if err := t.Remove(cleanupCtx, filepath); err != nil {
			Warnf("unable to remove temporary dump %s: %v", filepath, err)
		}
	}
}

type cleanupReadCloser struct {
	io.ReadCloser
	once    sync.Once
	cleanup func()
}

func (r *cleanupReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.cleanup)
	return err
}
