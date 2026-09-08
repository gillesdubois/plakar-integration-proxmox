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
	"fmt"
	"strconv"
	"strings"
)

// FreeSpaceMargin is kept free on top of the size of a transfer, so filling
// dump_dir never takes the node's filesystem down to its last block.
const FreeSpaceMargin = 256 << 20

// CheckCommands verifies the given Proxmox tools are reachable on the target.
//
// A missing binary then surfaces before any VM is touched, instead of as an
// opaque exit status in the middle of a job.
func (c *Client) CheckCommands(ctx context.Context, names ...string) error {
	for _, name := range names {
		if _, _, err := c.runner.Run(ctx, "sh", "-c", "command -v "+name+" >/dev/null 2>&1"); err != nil {
			return fmt.Errorf("%s not found on %s (mode=%s): is this host a Proxmox node?", name, c.cfg.Origin(), c.cfg.Mode)
		}
	}
	return nil
}

// CheckDumpDir verifies dump_dir exists and is writable.
//
// Without it a bad path is only reported once the archive has been streamed,
// because "cat > file" starts successfully whatever the destination is.
func (c *Client) CheckDumpDir(ctx context.Context) error {
	if _, _, err := c.runner.Run(ctx, "test", "-d", c.cfg.DumpDir); err != nil {
		return fmt.Errorf("dump_dir %s does not exist on %s", c.cfg.DumpDir, c.cfg.Origin())
	}
	if _, _, err := c.runner.Run(ctx, "test", "-w", c.cfg.DumpDir); err != nil {
		return fmt.Errorf("dump_dir %s is not writable on %s", c.cfg.DumpDir, c.cfg.Origin())
	}
	return nil
}

// AvailableBytes reports the free space of the filesystem holding dir.
func (c *Client) AvailableBytes(ctx context.Context, dir string) (int64, error) {
	stdout, stderr, err := c.runner.Run(ctx, "df", "-Pk", dir)
	if err != nil {
		return 0, fmt.Errorf("df failed for %s: %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	return parseDfAvailable(stdout)
}

// EnsureFreeSpace refuses a transfer that dir cannot hold.
//
// A failed space check is only a warning: it must never block a restore that
// would otherwise have worked, on a filesystem df cannot report on.
func (c *Client) EnsureFreeSpace(ctx context.Context, dir string, size int64) error {
	if size <= 0 {
		return nil
	}

	available, err := c.AvailableBytes(ctx, dir)
	if err != nil {
		Warnf("unable to check free space on %s: %v", dir, err)
		return nil
	}

	required := size + FreeSpaceMargin
	if available < required {
		return fmt.Errorf("not enough free space in %s: %s available, %s required",
			dir, humanBytes(available), humanBytes(required))
	}
	return nil
}

// parseDfAvailable reads the "Available" column of POSIX df output, in 1K units.
func parseDfAvailable(output string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %s", strings.TrimSpace(output))
	}

	last := strings.TrimSpace(lines[len(lines)-1])
	fields := strings.Fields(last)
	if len(fields) < 4 {
		return 0, fmt.Errorf("unexpected df output: %s", last)
	}

	blocks, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid df available value %q", fields[3])
	}
	return blocks * 1024, nil
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
