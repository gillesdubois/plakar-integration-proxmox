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
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
)

const qemuConfigDir = "/etc/pve/qemu-server"
const lxcConfigDir = "/etc/pve/lxc"

// BackupVMStream returns the archive name for a VM/CT and a reader over
// `vzdump --stdout`; nothing is written to the node. A vzdump failure
// surfaces as an error from the reader at EOF.
//
// vzdump only starts on the first Read. Started up front, it would take the
// node-wide vzdump lock while its record still waits in plakar's queue, and
// the importer would then block on the next VM's vzdump waiting for that lock.
func (c *Client) BackupVMStream(ctx context.Context, vmType string, vmid int) (string, io.ReadCloser, error) {
	baseExt, err := dumpBaseExtension(vmType)
	if err != nil {
		return "", nil, err
	}

	suffix, err := compressionSuffix(c.cfg.BackupCompression)
	if err != nil {
		return "", nil, err
	}

	args := []string{strconv.Itoa(vmid), "--stdout", "--mode", c.cfg.BackupMode, "--compress", c.cfg.BackupCompression}
	if c.cfg.Node != "" {
		args = append(args, "--node", c.cfg.Node)
	}

	timestamp := time.Now().Format("2006_01_02-15_04_05")
	archiveName := BuildDumpFilename(c.cfg, vmType, vmid, timestamp, baseExt, suffix)

	return archiveName, connectors.NewLazyReader(func() (io.ReadCloser, error) {
		return c.startVzdump(ctx, args)
	}), nil
}

func (c *Client) startVzdump(ctx context.Context, args []string) (io.ReadCloser, error) {
	c.vzdumpMu.Lock()
	stream, err := c.runner.Stream(ctx, "vzdump", args...)
	if err != nil {
		c.vzdumpMu.Unlock()
		return nil, fmt.Errorf("vzdump stream failed: %w", err)
	}

	stderrBuf := &bytes.Buffer{}
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		_, _ = io.Copy(stderrBuf, stream.Stderr)
	}()

	return &streamReadCloser{
		stdout:     stream.Stdout,
		finish:     stream.Finish,
		abort:      stream.Abort,
		release:    c.vzdumpMu.Unlock,
		stderr:     stderrBuf,
		stderrDone: doneCh,
	}, nil
}

func (c *Client) ReadQEMUConfig(ctx context.Context, vmid int) ([]byte, error) {
	return c.readVMConfig(ctx, "qemu", vmid)
}

func (c *Client) ReadLXCConfig(ctx context.Context, vmid int) ([]byte, error) {
	return c.readVMConfig(ctx, "lxc", vmid)
}

func VMConfigPath(vmType string, vmid int) (string, error) {
	switch vmType {
	case "qemu":
		return path.Join(qemuConfigDir, fmt.Sprintf("%d.conf", vmid)), nil
	case "lxc":
		return path.Join(lxcConfigDir, fmt.Sprintf("%d.conf", vmid)), nil
	default:
		return "", fmt.Errorf("unsupported VM type for config path: %s", vmType)
	}
}

func (c *Client) readVMConfig(ctx context.Context, vmType string, vmid int) ([]byte, error) {
	configPath, err := VMConfigPath(vmType, vmid)
	if err != nil {
		return nil, err
	}

	reader, err := c.Open(ctx, configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read %s config %s: %w", vmType, configPath, err)
	}
	defer func() { _ = reader.Close() }()

	configData, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("unable to read %s config content %s: %w", vmType, configPath, err)
	}

	return configData, nil
}

func dumpBaseExtension(vmType string) (string, error) {
	switch vmType {
	case "qemu":
		return "vma", nil
	case "lxc":
		return "tar", nil
	default:
		return "", fmt.Errorf("unsupported VM type: %s", vmType)
	}
}

// compressionSuffix maps vzdump's --compress value to the archive extension
// vzdump itself would use.
func compressionSuffix(compress string) (string, error) {
	switch compress {
	case "0":
		return "", nil
	case "1", "lzo":
		return ".lzo", nil
	case "gzip":
		return ".gz", nil
	case "zstd":
		return ".zst", nil
	default:
		return "", fmt.Errorf("unsupported backup_compression: %q", compress)
	}
}

type streamReadCloser struct {
	stdout     io.Reader
	finish     func() error
	abort      func() error
	release    func()
	eof        bool
	stderr     *bytes.Buffer
	stderrDone <-chan struct{}
	closed     bool
	finished   bool
	finishErr  error
}

func (r *streamReadCloser) Read(p []byte) (int, error) {
	n, err := r.stdout.Read(p)
	if err == io.EOF {
		r.eof = true
		if finishErr := r.finalize(); finishErr != nil {
			return n, finishErr
		}
	}
	return n, err
}

func (r *streamReadCloser) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	// Closed before EOF: vzdump would block writing to a pipe nobody reads,
	// and waiting for it would never return.
	if !r.eof && r.abort != nil {
		_ = r.abort()
	}
	return r.finalize()
}

func (r *streamReadCloser) finalize() error {
	if r.finished {
		return r.finishErr
	}
	r.finished = true

	var err error
	if r.finish != nil {
		err = r.finish()
	}
	if r.stderrDone != nil {
		<-r.stderrDone
	}
	if r.release != nil {
		r.release()
	}
	if err != nil {
		r.finishErr = fmt.Errorf("vzdump failed: %w: %s", err, strings.TrimSpace(r.stderr.String()))
	}
	return r.finishErr
}
