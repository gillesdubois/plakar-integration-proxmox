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
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	warnMu sync.Mutex
	warnTo io.Writer = os.Stderr
)

// Warnf reports a non-fatal condition to the operator.
//
// Connectors run as plugins that speak gRPC over stdin/stdout, so stderr is the
// only stream a connector can write to without corrupting the protocol.
func Warnf(format string, args ...any) {
	warnMu.Lock()
	defer warnMu.Unlock()
	fmt.Fprintf(warnTo, "proxmox: warning: "+format+"\n", args...)
}

// SetWarnWriter redirects warnings, for tests.
func SetWarnWriter(w io.Writer) func() {
	warnMu.Lock()
	previous := warnTo
	warnTo = w
	warnMu.Unlock()

	return func() {
		warnMu.Lock()
		warnTo = previous
		warnMu.Unlock()
	}
}
