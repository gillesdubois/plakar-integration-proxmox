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
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDetectCompressionSuffix(t *testing.T) {
	cases := map[string][]byte{
		".gz":  {0x1f, 0x8b, 0x08},
		".zst": {0x28, 0xb5, 0x2f, 0xfd, 0x00},
		".lzo": {0x89, 0x4c, 0x5a, 0x4f, 0x00, 0x0d, 0x0a, 0x1a, 0x0a},
		"":     []byte("VMA\x00"),
	}
	for want, header := range cases {
		if got := detectCompressionSuffix(header); got != want {
			t.Errorf("header %x: got %q, want %q", header, got, want)
		}
	}
}

func TestStreamReadCloserReportsVzdumpFailure(t *testing.T) {
	done := make(chan struct{})
	close(done)
	r := &streamReadCloser{
		stdout:     strings.NewReader("partial"),
		finish:     func() error { return errors.New("exit status 1") },
		stderr:     bytes.NewBufferString("ERROR: VM is locked\n"),
		stderrDone: done,
	}

	data, err := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "VM is locked") {
		t.Fatalf("expected vzdump failure with stderr, got data=%q err=%v", data, err)
	}
	if cerr := r.Close(); cerr == nil {
		t.Fatal("Close must keep reporting the vzdump failure")
	}
}
