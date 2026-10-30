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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/location"
	"github.com/gillesdubois/plakar-integration-proxmox/internal/proxmox"
)

type ProxmoxExporter struct {
	cfg         *proxmox.Config
	client      *proxmox.Client
	temp        *proxmox.TempFiles
	restoreOpts restoreOptions
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

type restoreOptions struct {
	startOnRestore bool
	forceVMRestore bool
	unique         bool
	uniqueSet      bool
	newID          int
	storage        string
	pool           string
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

	restoreOpts, err := parseRestoreOptions(config)
	if err != nil {
		return nil, err
	}

	client, err := proxmox.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &ProxmoxExporter{
		cfg:         cfg,
		client:      client,
		temp:        proxmox.NewTempFiles(client, cfg.Cleanup),
		restoreOpts: restoreOpts,
	}, nil
}

func (p *ProxmoxExporter) Origin() string        { return p.cfg.Origin() }
func (p *ProxmoxExporter) Type() string          { return protocolName }
func (p *ProxmoxExporter) Root() string          { return "/" }
func (p *ProxmoxExporter) Flags() location.Flags { return 0 }

func (p *ProxmoxExporter) Ping(ctx context.Context) error {
	if err := p.client.Ping(ctx); err != nil {
		return err
	}
	return p.preflight(ctx)
}

// preflight checks what a restore depends on before any archive is uploaded.
func (p *ProxmoxExporter) preflight(ctx context.Context) error {
	if err := p.client.CheckCommands(ctx, "pvesh", "qm", "pct", "qmrestore"); err != nil {
		return err
	}
	return p.client.CheckDumpDir(ctx)
}

func (p *ProxmoxExporter) Export(ctx context.Context, records <-chan *connectors.Record, results chan<- *connectors.Result) error {
	defer close(results)

	// Whatever is still tracked at this point was left behind by a failure
	// between the upload and the restore.
	defer p.temp.Sweep(ctx)

	if err := p.preflight(ctx); err != nil {
		// The records channel still has to be drained: the SDK blocks on it
		// until every record has been consumed and its reader closed.
		drainRecords(records, results, err)
		return err
	}

	sidecars := make(map[string]vmConfigSidecar)
	poolSidecars := make(map[string]string)
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
		if proxmox.IsPoolSidecarFilename(base) {
			if err := p.collectPoolSidecar(record, base, poolSidecars); err != nil {
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

		if err := p.client.EnsureFreeSpace(ctx, p.cfg.DumpDir, record.FileInfo.Lsize); err != nil {
			results <- record.Error(err)
			continue
		}

		dumpName := proxmox.BuildRestoreDumpFilename(base, vmType, vmid, time.Now())
		dumpPath := path.Join(p.cfg.DumpDir, dumpName)

		// Tracked before the write, so a partial upload is cleaned up too.
		p.temp.Track(dumpPath)
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
			poolName, poolErr := p.resolvePoolForDump(pending, poolSidecars)
			if poolErr != nil {
				err = poolErr
			} else {
				targetVMID := pending.vmid
				if p.restoreOpts.newID != 0 {
					targetVMID = p.restoreOpts.newID
				}

				err = p.restoreDump(ctx, pending.dumpPath, pending.vmType, targetVMID, configData, poolName)
			}
		}

		// Cleanup runs whether the restore worked or not: a restore that keeps
		// failing is exactly the one that would fill dump_dir. It is a no-op
		// when cleanup=false.
		if removeErr := p.temp.Remove(ctx, pending.dumpPath); removeErr != nil {
			proxmox.Warnf("unable to remove temporary dump %s: %v", pending.dumpPath, removeErr)
			if err == nil {
				err = removeErr
			}
		}

		results <- resultFromRecord(pending.record, err)
	}

	return nil
}

func (p *ProxmoxExporter) Close(ctx context.Context) error {
	p.temp.Sweep(ctx)
	return p.client.Close()
}

// drainRecords consumes and fails every remaining record, so the SDK's sender
// is never left blocked on a channel nobody reads.
func drainRecords(records <-chan *connectors.Record, results chan<- *connectors.Result, err error) {
	for record := range records {
		results <- record.Error(err)
	}
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

func (p *ProxmoxExporter) collectPoolSidecar(record *connectors.Record, sidecarBase string, sidecars map[string]string) error {
	dumpBase, err := proxmox.ParsePoolSidecarFilename(sidecarBase)
	if err != nil {
		return err
	}

	poolData, err := readRecordBytes(record)
	if err != nil {
		return err
	}
	sidecars[dumpBase] = strings.TrimSpace(string(poolData))
	return nil
}

func (p *ProxmoxExporter) resolvePoolForDump(pending pendingRestore, sidecars map[string]string) (string, error) {
	poolName, ok := sidecars[pending.dumpBase]
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(poolName), nil
}

func (p *ProxmoxExporter) restoreDump(ctx context.Context, dumpPath, vmType string, vmid int, configData []byte, poolName string) error {
	state, err := p.vmState(ctx, vmType, vmid)
	if err != nil {
		return err
	}

	if state.exists {
		// Restoring over an existing VM/CT destroys its current disks and
		// cannot be undone, so it stays behind an explicit opt-in whatever its
		// runtime state is. A stopped VM used to be overwritten silently.
		if !p.restoreOpts.forceVMRestore {
			return fmt.Errorf("refusing restore for %s %d: it already exists on %s (pass -o force_vm_restore=true to overwrite it, or -o newid=<id> to restore next to it)", vmType, vmid, p.cfg.Origin())
		}

		if state.running {
			if err := p.stopVM(ctx, vmType, vmid); err != nil {
				return err
			}
			state, err = p.vmState(ctx, vmType, vmid)
			if err != nil {
				return err
			}
			if state.running {
				return fmt.Errorf("refusing restore for %s %d: VM/CT is still running after stop request", vmType, vmid)
			}
		}
	}

	opts, err := p.resolveRestoreOptions(ctx, vmType, state.exists, configData, poolName)
	if err != nil {
		return err
	}

	if err := p.runRestoreDump(ctx, dumpPath, vmType, vmid, opts); err != nil {
		return err
	}

	if opts.unique && vmType == "qemu" {
		if err := p.regenerateSMBIOSUUID(ctx, vmid); err != nil {
			return err
		}
	}

	if p.restoreOpts.startOnRestore {
		if err := p.startVM(ctx, vmType, vmid); err != nil {
			return err
		}
	}

	return nil
}

func (p *ProxmoxExporter) resolveRestoreOptions(ctx context.Context, vmType string, targetExists bool, configData []byte, poolName string) (restoreOptions, error) {
	opts := p.restoreOpts

	// A restore under a new VMID is a copy living next to its source, so it must
	// not come up holding the same identity on the same network.
	if !opts.uniqueSet {
		opts.unique = opts.newID != 0
	}

	if !targetExists {
		if opts.storage == "" {
			opts.storage = parseStorageFromConfig(vmType, configData)
		}
		if opts.pool == "" && poolName != "" {
			exists, err := p.client.PoolExists(ctx, poolName)
			if err != nil {
				return restoreOptions{}, err
			}
			if exists {
				opts.pool = poolName
			} else {
				proxmox.Warnf("pool %q recorded in the backup no longer exists on %s: the restored VM/CT will not belong to any pool (use -o pool=<name> to pick another one)", poolName, p.cfg.Origin())
			}
		}
	}

	if opts.pool != "" {
		exists, err := p.client.PoolExists(ctx, opts.pool)
		if err != nil {
			return restoreOptions{}, err
		}
		if !exists {
			return restoreOptions{}, fmt.Errorf("restore pool does not exist: %s", opts.pool)
		}
	}

	return opts, nil
}

func (p *ProxmoxExporter) runRestoreDump(ctx context.Context, dumpPath, vmType string, vmid int, opts restoreOptions) error {
	vmidStr := strconv.Itoa(vmid)
	var cmd string
	var args []string
	switch vmType {
	case "qemu":
		cmd = "qmrestore"
		args = []string{dumpPath, vmidStr}
	case "lxc":
		cmd = "pct"
		args = []string{"restore", vmidStr, dumpPath}
	default:
		return fmt.Errorf("unsupported backup type: %s", vmType)
	}

	if opts.forceVMRestore {
		args = append(args, "--force")
	}
	if opts.unique {
		args = append(args, "--unique")
	}
	if opts.storage != "" {
		args = append(args, "--storage", opts.storage)
	}
	if opts.pool != "" {
		args = append(args, "--pool", opts.pool)
	}

	_, stderr, err := p.client.Run(ctx, cmd, args...)
	if err != nil {
		return fmt.Errorf("restore failed: %w: %s", err, strings.TrimSpace(stderr))
	}

	return nil
}

// regenerateSMBIOSUUID gives a restored QEMU VM its own SMBIOS UUID.
//
// --unique only randomises MAC addresses; guest software and inventories keyed
// on the SMBIOS UUID would still see two identical machines.
func (p *ProxmoxExporter) regenerateSMBIOSUUID(ctx context.Context, vmid int) error {
	configData, err := p.client.ReadQEMUConfig(ctx, vmid)
	if err != nil {
		return fmt.Errorf("restore of qemu %d succeeded but its smbios settings could not be read: %w", vmid, err)
	}

	smbios, err := uniqueSMBIOS(configData)
	if err != nil {
		return fmt.Errorf("restore of qemu %d succeeded but a new smbios uuid could not be generated: %w", vmid, err)
	}

	if _, stderr, err := p.client.Run(ctx, "qm", "set", strconv.Itoa(vmid), "--smbios1", smbios); err != nil {
		return fmt.Errorf("restore of qemu %d succeeded but its smbios uuid could not be replaced: %w: %s", vmid, err, strings.TrimSpace(stderr))
	}
	return nil
}

// uniqueSMBIOS rebuilds the smbios1 property string around a fresh uuid, keeping
// every other field the VM already declared.
func uniqueSMBIOS(configData []byte) (string, error) {
	uuid, err := randomUUID()
	if err != nil {
		return "", err
	}

	var current string
	for _, entry := range activeConfigEntries(configData) {
		if entry[0] == "smbios1" {
			current = entry[1]
			break
		}
	}
	if current == "" {
		return "uuid=" + uuid, nil
	}

	fields := strings.Split(current, ",")
	for i, field := range fields {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(field)), "uuid=") {
			fields[i] = "uuid=" + uuid
			return strings.Join(fields, ","), nil
		}
	}
	return strings.Join(append(fields, "uuid="+uuid), ","), nil
}

func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
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

	return p.waitUntilVMStopped(ctx, vmType, vmid)
}

func (p *ProxmoxExporter) waitUntilVMStopped(ctx context.Context, vmType string, vmid int) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout while waiting for %s %d to stop", vmType, vmid)
		}

		state, err := p.vmState(ctx, vmType, vmid)
		if err != nil {
			return err
		}
		if !state.running {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
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

func isIgnorableStartError(output string) bool {
	normalized := strings.ToLower(output)
	return strings.Contains(normalized, "already running")
}

func isIgnorableStopError(output string) bool {
	normalized := strings.ToLower(output)
	if strings.Contains(normalized, "already stopped") || strings.Contains(normalized, "already down") {
		return true
	}
	return isMissingVMError(output)
}

func parseBoolOption(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid boolean value: %s", value)
	}
	return parsed, nil
}

func parseRestoreOptions(config map[string]string) (restoreOptions, error) {
	var opts restoreOptions

	startOnRestore, err := parseBoolOption(config["start_on_restore"])
	if err != nil {
		return restoreOptions{}, err
	}
	opts.startOnRestore = startOnRestore

	forceVMRestore, err := parseBoolOption(config["force_vm_restore"])
	if err != nil {
		return restoreOptions{}, err
	}
	opts.forceVMRestore = forceVMRestore

	// Left unset, uniqueness follows newid: see resolveRestoreOptions.
	if uniqueRaw, ok := config["unique"]; ok && strings.TrimSpace(uniqueRaw) != "" {
		unique, err := parseBoolOption(uniqueRaw)
		if err != nil {
			return restoreOptions{}, fmt.Errorf("invalid unique value: %s", strings.TrimSpace(uniqueRaw))
		}
		opts.unique = unique
		opts.uniqueSet = true
	}

	opts.storage = strings.TrimSpace(config["storage"])
	opts.pool = strings.TrimSpace(config["pool"])

	newIDRaw, hasNewID := config["newid"]
	if hasNewID {
		newIDRaw = strings.TrimSpace(newIDRaw)
		if newIDRaw != "" {
			newID, err := strconv.Atoi(newIDRaw)
			if err != nil {
				return restoreOptions{}, fmt.Errorf("invalid newid value: %s", newIDRaw)
			}
			if newID <= 0 {
				return restoreOptions{}, fmt.Errorf("newid must be a positive integer: %s", newIDRaw)
			}
			opts.newID = newID
		}
	}

	return opts, nil
}

// missingVMPatterns match the ways Proxmox says a VM/CT is not on this node.
var missingVMPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)configuration file '[^']*' does not exist`),
	regexp.MustCompile(`(?i)unable to find configuration file for (vm|ct) \d+`),
	regexp.MustCompile(`(?i)\b(vm|ct|container) \d+ does not exist\b`),
	regexp.MustCompile(`(?i)\bno such (vm|container)\b`),
}

// isMissingVMError tells "the target is not there" apart from any other failure.
//
// Matching a bare "does not exist", or the words "configuration file" on their
// own, was far too loose: a permission problem or a pmxcfs that lost quorum
// mentions those too, and reading such an error as "the VMID is free" leads
// straight to overwriting a VM that does exist.
func isMissingVMError(output string) bool {
	if strings.TrimSpace(output) == "" {
		return false
	}

	for _, pattern := range missingVMPatterns {
		if pattern.MatchString(output) {
			return true
		}
	}
	return false
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

// qemuDiskKeyRegex matches the QEMU config keys that carry a guest disk.
//
// efidisk0 and tpmstate0 deliberately fall outside it: they are small auxiliary
// volumes that say nothing about where the VM's actual disks belong.
var qemuDiskKeyRegex = regexp.MustCompile(`^(scsi|virtio|sata|ide|nvme)\d+$`)

// parseStorageFromConfig picks the storage a restore should target, from the
// config sidecar kept alongside the dump.
func parseStorageFromConfig(vmType string, configData []byte) string {
	if len(configData) == 0 {
		return ""
	}

	switch vmType {
	case "qemu":
		return parseQEMUStorage(configData)
	case "lxc":
		return parseLXCStorage(configData)
	default:
		return ""
	}
}

// activeConfigEntries returns the key/value pairs of the live configuration.
//
// A Proxmox config file lists one "[snapname]" section per snapshot after the
// active configuration, each with its own disk lines; reading past the first
// section would resolve a storage the VM does not use any more.
func activeConfigEntries(configData []byte) [][2]string {
	entries := make([][2]string, 0)

	for _, line := range strings.Split(string(configData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}

		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		entries = append(entries, [2]string{
			strings.TrimSpace(strings.ToLower(key)),
			strings.TrimSpace(value),
		})
	}

	return entries
}

// parseQEMUStorage resolves the storage holding the VM's boot disk.
//
// Returning the first disk-looking line instead used to pick whatever sorted
// first in the file: efidisk0 on a UEFI VM, and the ISO datastore of
// "ide2: local:iso/...,media=cdrom" on a BIOS one. Since the result is passed
// as --storage, which relocates every disk, that sent VMs to the wrong storage.
func parseQEMUStorage(configData []byte) string {
	disks := make(map[string]string)
	keys := make([]string, 0)
	var bootOrder []string

	for _, entry := range activeConfigEntries(configData) {
		key, value := entry[0], entry[1]

		if key == "boot" {
			bootOrder = parseBootOrder(value)
			continue
		}
		if !qemuDiskKeyRegex.MatchString(key) || isCDROMVolume(value) {
			continue
		}

		storage := parseStorageFromVolumeSpec(value)
		if storage == "" {
			continue
		}
		disks[key] = storage
		keys = append(keys, key)
	}

	for _, device := range bootOrder {
		if storage, ok := disks[device]; ok {
			return storage
		}
	}

	// No usable boot order: fall back to the first data disk, by device name.
	sort.Strings(keys)
	if len(keys) > 0 {
		return disks[keys[0]]
	}
	return ""
}

func parseLXCStorage(configData []byte) string {
	for _, entry := range activeConfigEntries(configData) {
		if entry[0] == "rootfs" {
			return parseStorageFromVolumeSpec(entry[1])
		}
	}
	return ""
}

// parseBootOrder reads the devices of a "boot: order=scsi0;ide2" line.
//
// The legacy letter form ("boot: cdn") names device classes rather than
// devices, and carries nothing usable here.
func parseBootOrder(value string) []string {
	for _, part := range strings.Split(value, ",") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(part), "order=")
		if !ok {
			continue
		}

		devices := make([]string, 0)
		for _, device := range strings.Split(rest, ";") {
			if device = strings.ToLower(strings.TrimSpace(device)); device != "" {
				devices = append(devices, device)
			}
		}
		return devices
	}
	return nil
}

func isCDROMVolume(spec string) bool {
	for _, part := range strings.Split(spec, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "media=cdrom") {
			return true
		}
	}
	return false
}

func parseStorageFromVolumeSpec(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}

	volume := strings.Split(spec, ",")[0]
	volume = strings.TrimSpace(volume)
	if volume == "" {
		return ""
	}

	storage, _, ok := strings.Cut(volume, ":")
	if !ok {
		return ""
	}
	storage = strings.TrimSpace(storage)
	if storage == "" {
		return ""
	}

	// Ignore explicit "none" values used in some optional disk entries.
	if strings.EqualFold(storage, "none") {
		return ""
	}
	return storage
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
