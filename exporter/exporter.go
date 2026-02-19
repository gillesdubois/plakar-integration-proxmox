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

package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/location"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/proxmox"
)

type ProxmoxExporter struct {
	cfg    *proxmox.Config
	client *proxmox.Client
}

type vmConfigSidecar struct {
	vmType string
	data   []byte
}

type pendingRestore struct {
	record   *connectors.Record
	vmType   string
	vmid     int
	dumpBase string
	dumpPath string
}

type vmRuntimeState struct {
	exists  bool
	running bool
}

const protocolName = "proxmox+backup"

func init() {
	if err := exporter.Register(protocolName, 0, NewProxmoxExporter); err != nil {
		panic(err)
	}
}

func NewProxmoxExporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (exporter.Exporter, error) {
	cfg, err := proxmox.ParseConfig(config)
	if err != nil {
		return nil, err
	}

	client, err := proxmox.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &ProxmoxExporter{cfg: cfg, client: client}, nil
}

func (p *ProxmoxExporter) Origin() string        { return p.cfg.Origin() }
func (p *ProxmoxExporter) Type() string          { return protocolName }
func (p *ProxmoxExporter) Root() string          { return "/" }
func (p *ProxmoxExporter) Flags() location.Flags { return 0 }

func (p *ProxmoxExporter) Ping(ctx context.Context) error {
	return p.client.Ping(ctx)
}

func (p *ProxmoxExporter) Export(ctx context.Context, records <-chan *connectors.Record, results chan<- *connectors.Result) error {
	defer close(results)

	sidecars := make(map[string]vmConfigSidecar)
	pendingRestores := make([]pendingRestore, 0)

	for record := range records {
		if err := ctx.Err(); err != nil {
			results <- record.Error(err)
			continue
		}

		if record.Err != nil || record.IsXattr || !record.FileInfo.Lmode.IsRegular() {
			results <- record.Ok()
			continue
		}

		base := path.Base(record.Pathname)
		if proxmox.IsConfigSidecarFilename(base) {
			if err := p.collectConfigSidecar(record, base, sidecars); err != nil {
				_ = closeRecord(record)
				results <- resultFromRecord(record, err)
				continue
			}
			results <- resultFromRecord(record, nil)
			continue
		}

		vmType, vmid, err := proxmox.ParseDumpFilename(base)
		if err != nil {
			if strings.HasPrefix(base, "vzdump-") {
				results <- record.Error(err)
				continue
			}
			results <- record.Ok()
			continue
		}

		dumpName := proxmox.BuildRestoreDumpFilename(base, vmType, vmid, time.Now())
		dumpPath := path.Join(p.cfg.DumpDir, dumpName)
		if err := p.writeDump(ctx, dumpPath, record.Reader); err != nil {
			results <- record.Error(err)
			continue
		}

		if err := closeRecord(record); err != nil {
			results <- resultFromRecord(record, err)
			continue
		}

		pendingRestores = append(pendingRestores, pendingRestore{
			record:   record,
			vmType:   vmType,
			vmid:     vmid,
			dumpBase: base,
			dumpPath: dumpPath,
		})
	}

	for _, pending := range pendingRestores {
		if err := ctx.Err(); err != nil {
			results <- resultFromRecord(pending.record, err)
			continue
		}

		configData, err := p.resolveConfigForDump(pending, sidecars)
		if err == nil {
			err = p.restoreDump(ctx, pending.dumpPath, pending.vmType, pending.vmid, configData)
		}

		if err == nil && p.cfg.Cleanup {
			if removeErr := p.client.Remove(ctx, pending.dumpPath); removeErr != nil {
				err = removeErr
			}
		}

		results <- resultFromRecord(pending.record, err)
	}

	return nil
}

func (p *ProxmoxExporter) Close(ctx context.Context) error {
	return p.client.Close()
}

func (p *ProxmoxExporter) writeDump(ctx context.Context, dumpPath string, reader io.Reader) error {
	writer, err := p.client.Create(ctx, dumpPath)
	if err != nil {
		return err
	}

	if _, err := io.Copy(writer, reader); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func (p *ProxmoxExporter) collectConfigSidecar(record *connectors.Record, sidecarBase string, sidecars map[string]vmConfigSidecar) error {
	dumpBase, vmType, err := proxmox.ParseConfigSidecarFilename(sidecarBase)
	if err != nil {
		return err
	}

	configData, err := readRecordBytes(record)
	if err != nil {
		return err
	}

	sidecars[dumpBase] = vmConfigSidecar{
		vmType: vmType,
		data:   configData,
	}
	return nil
}

func (p *ProxmoxExporter) resolveConfigForDump(pending pendingRestore, sidecars map[string]vmConfigSidecar) ([]byte, error) {
	sidecar, ok := sidecars[pending.dumpBase]
	if !ok {
		return nil, nil
	}
	if sidecar.vmType != pending.vmType {
		return nil, fmt.Errorf("config sidecar type mismatch for dump %s: got %s, expected %s", pending.dumpBase, sidecar.vmType, pending.vmType)
	}
	return sidecar.data, nil
}

func (p *ProxmoxExporter) restoreDump(ctx context.Context, dumpPath, vmType string, vmid int, configData []byte) error {
	state, err := p.vmState(ctx, vmType, vmid)
	if err != nil {
		return err
	}

	shouldStartAfterRestore := false
	if state.exists {
		shouldStartAfterRestore = state.running
		if err := p.stopVM(ctx, vmType, vmid); err != nil {
			return err
		}
	} else {
		if len(configData) == 0 {
			return fmt.Errorf("%s %d does not exist and no config sidecar was provided", vmType, vmid)
		}
		if err := p.writeVMConfig(ctx, vmType, vmid, configData); err != nil {
			return err
		}
		shouldStartAfterRestore = true
	}

	restoreErr := p.runRestoreDump(ctx, dumpPath, vmType, vmid)
	if restoreErr != nil {
		if state.exists && state.running {
			if startErr := p.startVM(ctx, vmType, vmid); startErr != nil {
				return errors.Join(restoreErr, fmt.Errorf("failed to restore previous running state: %w", startErr))
			}
		}
		return restoreErr
	}

	if shouldStartAfterRestore {
		if err := p.startVM(ctx, vmType, vmid); err != nil {
			return err
		}
	}

	return nil
}

func (p *ProxmoxExporter) runRestoreDump(ctx context.Context, dumpPath, vmType string, vmid int) error {
	vmidStr := strconv.Itoa(vmid)
	var cmd string
	var args []string
	switch vmType {
	case "qemu":
		cmd = "qmrestore"
		args = []string{dumpPath, vmidStr, "--force"}
	case "lxc":
		cmd = "pct"
		args = []string{"restore", vmidStr, dumpPath, "--force"}
	default:
		return fmt.Errorf("unsupported backup type: %s", vmType)
	}

	_, stderr, err := p.client.Run(ctx, cmd, args...)
	if err != nil {
		return fmt.Errorf("restore failed: %w: %s", err, strings.TrimSpace(stderr))
	}

	return nil
}

func (p *ProxmoxExporter) vmState(ctx context.Context, vmType string, vmid int) (vmRuntimeState, error) {
	cmd, err := vmCommand(vmType)
	if err != nil {
		return vmRuntimeState{}, err
	}

	vmidStr := strconv.Itoa(vmid)
	stdout, stderr, err := p.client.Run(ctx, cmd, "status", vmidStr)
	output := preferredOutput(stdout, stderr)
	if err != nil {
		if isMissingVMError(output) {
			return vmRuntimeState{exists: false, running: false}, nil
		}
		return vmRuntimeState{}, fmt.Errorf("status failed for %s %d: %w: %s", vmType, vmid, err, output)
	}

	status := parseStatusValue(stdout + "\n" + stderr)
	switch status {
	case "running", "paused", "suspended":
		return vmRuntimeState{exists: true, running: true}, nil
	case "stopped":
		return vmRuntimeState{exists: true, running: false}, nil
	default:
		return vmRuntimeState{}, fmt.Errorf("unable to parse status for %s %d: %s", vmType, vmid, preferredOutput(stdout, stderr))
	}
}

func (p *ProxmoxExporter) writeVMConfig(ctx context.Context, vmType string, vmid int, configData []byte) error {
	configPath, err := proxmox.VMConfigPath(vmType, vmid)
	if err != nil {
		return err
	}

	writer, err := p.client.Create(ctx, configPath)
	if err != nil {
		return err
	}

	if _, err := writer.Write(configData); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func (p *ProxmoxExporter) stopVM(ctx context.Context, vmType string, vmid int) error {
	cmd, err := vmCommand(vmType)
	if err != nil {
		return err
	}

	vmidStr := strconv.Itoa(vmid)
	stdout, stderr, err := p.client.Run(ctx, cmd, "stop", vmidStr)
	if err != nil {
		output := preferredOutput(stdout, stderr)
		if isIgnorableStopError(output) {
			return nil
		}
		return fmt.Errorf("stop failed for %s %d: %w: %s", vmType, vmid, err, output)
	}

	return nil
}

func (p *ProxmoxExporter) startVM(ctx context.Context, vmType string, vmid int) error {
	cmd, err := vmCommand(vmType)
	if err != nil {
		return err
	}

	vmidStr := strconv.Itoa(vmid)
	stdout, stderr, err := p.client.Run(ctx, cmd, "start", vmidStr)
	if err != nil {
		output := preferredOutput(stdout, stderr)
		if isIgnorableStartError(output) {
			return nil
		}
		return fmt.Errorf("start failed for %s %d: %w: %s", vmType, vmid, err, output)
	}

	return nil
}

func vmCommand(vmType string) (string, error) {
	switch vmType {
	case "qemu":
		return "qm", nil
	case "lxc":
		return "pct", nil
	default:
		return "", fmt.Errorf("unsupported backup type: %s", vmType)
	}
}

func isIgnorableStopError(output string) bool {
	normalized := strings.ToLower(output)
	return isMissingVMError(normalized) ||
		strings.Contains(normalized, "not running") ||
		strings.Contains(normalized, "already stopped")
}

func isIgnorableStartError(output string) bool {
	normalized := strings.ToLower(output)
	return strings.Contains(normalized, "already running")
}

func isMissingVMError(output string) bool {
	if output == "" {
		return false
	}
	normalized := strings.ToLower(output)
	return strings.Contains(normalized, "does not exist") ||
		strings.Contains(normalized, "no such vm") ||
		strings.Contains(normalized, "no such container") ||
		strings.Contains(normalized, "configuration file")
}

func preferredOutput(stdout, stderr string) string {
	output := strings.TrimSpace(stderr)
	if output == "" {
		output = strings.TrimSpace(stdout)
	}
	return output
}

func parseStatusValue(output string) string {
	for _, line := range strings.Split(strings.ToLower(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "status:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "status:"))
		}
	}
	return ""
}

func readRecordBytes(record *connectors.Record) ([]byte, error) {
	if record.Reader == nil {
		return nil, fmt.Errorf("missing record reader for %s", record.Pathname)
	}

	data, readErr := io.ReadAll(record.Reader)
	closeErr := closeRecord(record)
	if readErr != nil && closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	return data, nil
}

func closeRecord(record *connectors.Record) error {
	if record.Reader == nil {
		return nil
	}
	err := record.Close()
	record.Reader = nil
	return err
}

func resultFromRecord(record *connectors.Record, err error) *connectors.Result {
	return &connectors.Result{
		Record: *record,
		Err:    err,
	}
}
