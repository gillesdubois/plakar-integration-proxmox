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
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompressionSuffix(t *testing.T) {
	cases := map[string]string{"0": "", "1": ".lzo", "lzo": ".lzo", "gzip": ".gz", "zstd": ".zst"}
	for compress, want := range cases {
		if got, err := compressionSuffix(compress); err != nil || got != want {
			t.Errorf("%q: got %q, %v, want %q", compress, got, err, want)
		}
	}
	if _, err := compressionSuffix("bzip2"); err == nil {
		t.Error("unknown compression must be rejected")
	}
}

// streamRunner counts the vzdump streams it starts; every other call fails.
type streamRunner struct {
	Runner
	started int32
}

func (r *streamRunner) Stream(ctx context.Context, name string, args ...string) (*CommandStream, error) {
	atomic.AddInt32(&r.started, 1)
	return &CommandStream{
		Stdout: strings.NewReader("VMA\x00data"),
		Stderr: strings.NewReader(""),
	}, nil
}

func TestBackupVMStreamStartsVzdumpOnFirstRead(t *testing.T) {
	runner := &streamRunner{}
	client := &Client{cfg: &Config{BackupMode: "snapshot", BackupCompression: "zstd"}, runner: runner}

	name, reader, err := client.BackupVMStream(context.Background(), "qemu", 301)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "vzdump-qemu-301-") || !strings.HasSuffix(name, ".vma.zst") {
		t.Errorf("unexpected archive name %q", name)
	}
	if atomic.LoadInt32(&runner.started) != 0 {
		t.Fatal("vzdump must not start before the first Read")
	}

	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "VMA\x00data" || atomic.LoadInt32(&runner.started) != 1 {
		t.Fatalf("got data=%q err=%v started=%d", data, err, atomic.LoadInt32(&runner.started))
	}
	_ = reader.Close()

	// A record plakar never reads must not start vzdump at all.
	_, unread, _ := client.BackupVMStream(context.Background(), "lxc", 500)
	if err := unread.Close(); err != nil || atomic.LoadInt32(&runner.started) != 1 {
		t.Fatalf("closing an unread stream: err=%v started=%d", err, atomic.LoadInt32(&runner.started))
	}
}

func TestOneVzdumpAtATime(t *testing.T) {
	runner := &streamRunner{}
	client := &Client{cfg: &Config{BackupMode: "snapshot", BackupCompression: "0"}, runner: runner}

	_, first, _ := client.BackupVMStream(context.Background(), "qemu", 301)
	_, second, _ := client.BackupVMStream(context.Background(), "qemu", 302)

	if _, err := first.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(second)
		secondDone <- err
	}()

	select {
	case <-secondDone:
		t.Fatal("second vzdump started while the first one was still streaming")
	case <-time.After(50 * time.Millisecond):
	}

	_ = first.Close()
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&runner.started); got != 2 {
		t.Fatalf("started %d vzdump, want 2", got)
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
