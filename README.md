# Proxmox Integration

## Overview

**Proxmox** (PVE) is a complete open-source platform for enterprise virtualization. With the built-in web interface you can easily manage VMs and containers, software-defined storage and networking, high-availability clustering, and multiple out-of-the-box tools using a single solution.

This integration allows:

* **Backup of virtual machines and containers:**
  Backup and store vzdump coming from Proxmox virtual machines and containers.

* **Virtual machines and containers restoration:**
  Restore previously backed-up virtual machines and containers dumps directly into a proxmox instance.

## Configuration

The configuration parameters are as follows:
- `mode` (required): Define how backup will be done, can be either `local` or `remote` : 
    - `local` : Plakar is installed directly on the proxmox instance
    - `remote`: Plakar is installed on a remote instance and need to connect in order to perform the backup
- `conn_method` (required if mode : `remote`): Set how user will connect to the remote server : 
    - `password` : Plakar will use standard ssh username / password combo to login
    - `identity` : Plakar will use a private key to connect with the set username
- `conn_username` (required if mode : `remote`): Proxmox user that will be used to connect and perform backup
- `conn_password` (required if conn_method : `password` ): Password that will be used to connect remotely and perform the backup
- `conn_identity_file` (required if conn_method : `identity` ): Identitfy key file path used to connect
- `backup_compression` (optional): Backup compression mode used by proxmox when dumping the VM / CT (defaults to `0`) :
    - `0` : No compression applied
    - `1` : Proxmox default compression
    - `lzo` : LZO compression applied 
    - `gzip` : GZIP compression applied
    - `zstd` : ZSTD compression applied 
- `backup_mode` (optional): Backup mode used, will impact how VM / CT behave during backup (defaults to `snapshot`) : 
    - `snapshot` : Use a snapshot mode without stopping or suspending VM / CT
    - `suspend` : VM or CT will be suspended during the backup
    - `stop` : Proxmox will stop the VM / CT in order to perform the backup
- `dump_dir` (optional): Directory used by Proxmox to store dump archives (defaults to `/var/lib/vz/dump`). It is used for restore uploads and for backup generation in both modes.
- `node` (optional): Proxmox node to target for restore/upload operations (required if your cluster has multiple nodes)
- `cleanup` (optional): When `true`, delete temporary vzdump files from Proxmox storage after restore and after backups (defaults to `true`).
- `ssh_retry_count` (optional, only used when `mode=remote`): Number of retries when an SSH action fails to open a channel/session (e.g. `ssh: rejected: connect failed (open failed)` on a stale connection). Defaults to `3`. Set to `0` to disable retries.
- `ssh_retry_delay` (optional, only used when `mode=remote`): Delay to wait between SSH retries, as a Go duration (e.g. `2s`, `500ms`, `1m`). Defaults to `2s`.
- `ssh_known_hosts` (optional, only used when `mode=remote`): Path to the `known_hosts` file used to verify the Proxmox node host key (defaults to `~/.ssh/known_hosts`).
- `ssh_insecure_ignore_host_key` (optional, only used when `mode=remote`): When `true`, skip host key verification entirely (defaults to `false`).

## Restore behavior and options

During restore, the exporter checks whether the target VM/CT exists and its runtime state:

- **If the target VMID already exists**: restore is refused unless `-o force_vm_restore=true`.
  Restoring over an existing VM/CT destroys its current disks and cannot be undone, so it is
  always an explicit choice, whether the VM/CT is running or stopped. With the option set, a
  running VM/CT is stopped first, then overwritten.
- **If it does not exist**: restore is performed from the dump. When a matching sidecar config
  file (`_qemu.conf` or `_lxc.conf`) is available, it is used as a storage hint. When a matching
  pool sidecar (`_pool.conf`) is available, the exporter checks that the pool still exists and
  then passes `--pool <pool>`; if the pool is gone, a warning is printed and the VM/CT is
  restored without pool membership.
- **After a successful restore**: the VM/CT is started when `-o start_on_restore=true`.
- **Storage / pool override**:
  - `-o storage=<name>` forces the storage target used by restore, overriding the sidecar hint.
  - `-o pool=<name>` forces the pool used by restore, overriding the sidecar hint.

Restore options are passed via the generic `-o` flag of `plakar restore`:

- `start_on_restore=true|false` (`false` by default): start restored VM/CT after success.
- `force_vm_restore=true|false` (`false` by default): allow the restore to overwrite an existing
  VM/CT, stopping it first if it is running. Without it, a restore onto an existing VMID is refused.
- `unique=true|false`: give the restored VM/CT a new identity instead of a copy of the source one:
  random MAC addresses on every interface, and for QEMU a fresh SMBIOS UUID. **Defaults to `true`
  when `newid` is set**, `false` otherwise. Set it explicitly to override that default.
- `storage=<name>`: force target storage for restore.
- `pool=<name>`: force target pool for restore.
- `newid=<id>`: restore under another VMID than the one contained in the source dump.

### About the storage hint

`--storage` is not a per-disk setting: Proxmox applies it to **every** disk of the restored
VM/CT. The sidecar hint therefore resolves a single storage, the one holding the guest's boot
disk, following the `boot: order=` line of the saved configuration. CD-ROM entries
(`media=cdrom`), EFI disks and TPM state are ignored: they say nothing about where the guest's
actual disks belong.

Recommended practice:

- Restoring onto the **same cluster** it was backed up from: let the hint do its job, pass nothing.
- Restoring onto a **different cluster**, or a node whose storage is named differently: pass
  `-o storage=<name>` explicitly. The hint names a storage of the source cluster, which may not
  exist on the target, and a restore that silently lands on the wrong storage is worse than one
  that fails.
- VMs whose disks are **spread over several storages**: the restore consolidates them onto one.
  Pass `-o storage=` deliberately, then move the disks back afterwards if needed.

## Backup selection options

Backup selection is passed via the generic `-o` flag of `plakar backup` and is forwarded to the importer as key/value options.
You should set exactly one of the following:

- `vmid=<id>`: backup a single VM/CT
- `pool=<name>`: backup all VMs/CTs in a pool
- `all` or `all=true`: backup everything

## Disk Space and Cache Management

When backing up large virtual machines or containers, temporary files and cache metadata can consume a significant amount of local disk space.

There are two main areas of disk usage to consider:

1. **Proxmox VZDump Temporary Files**:
   By default, Proxmox creates a temporary dump in the directory configured by `dump_dir` (default: `/var/lib/vz/dump`). If `cleanup` is set to `true` (default), these files are automatically deleted after Plakar finishes importing them.

2. **Plakar Cache and Stage Packfiles**:
   Plakar stores metadata and indexes in a local cache (by default under `~/.cache/plakar`) and creates temporary "packfiles" on disk during backup operations before sending them to the repository.
   When backing up large VMs, this cache and the temporary packfiles can quickly fill up the system partition. You can redirect them to a larger mount point using the following `plakar backup` subcommand flags:
   - `-cache <path>`: Specifies a custom directory for the VFS cache. Set to `no` to disable caching, or `vfs` (default) for in-memory caching.
   - `-packfiles <path>`: Specifies a directory where temporary packfiles will be staged. Set to `memory` (default) to build them entirely in RAM.

## Backup File Structure

Each backed-up VM/CT produces a dump object under `/backup/<type>/<vmid>_<vmname>/`:
- `/backup/<type>/<vmid>_<vmname>/vzdump-<type>-<vmid>-<timestamp>.<ext>[.gz|.zst|.lzo]`

For VM configs, sidecar files are also added:
- `/backup/<type>/<vmid>_<vmname>/vzdump-<type>-<vmid>-<timestamp>.<ext>[.gz|.zst|.lzo]_qemu.conf`
- `/backup/<type>/<vmid>_<vmname>/vzdump-<type>-<vmid>-<timestamp>.<ext>[.gz|.zst|.lzo]_lxc.conf`
- `/backup/<type>/<vmid>_<vmname>/vzdump-<type>-<vmid>-<timestamp>.<ext>[.gz|.zst|.lzo]_pool.conf`

## Backup Example

Example for a QEMU VM with `vmid=101` named `myvm` compressed with zstd:

```text
/backup/qemu/101_myvm/vzdump-qemu-101-2026_02_10-02_00_00.vma.zst
```

## Examples

```bash
# Configure a Proxmox local source
$ plakar source add myProxmoxHypervisorLocal proxmox+backup://10.0.0.10 mode=local

# Configure a Proxmox remote source (with password auth)
$ plakar source add myProxmoxHypervisorRemote proxmox+backup://10.0.0.10 mode=remote conn_username=root conn_password=aSecureAndStrongPass conn_method=password

# Configure a Proxmox remote source (with identity auth)
$ plakar source add myProxmoxHypervisorRemote proxmox+backup://10.0.0.10 mode=remote conn_username=root conn_identity_file=/path/to/somewhere/pmx_id conn_method=identity

# Configure a Proxmox remote source with custom SSH retry behavior
$ plakar source add myProxmoxHypervisorRemote proxmox+backup://10.0.0.10 mode=remote conn_username=root conn_identity_file=/path/to/somewhere/pmx_id conn_method=identity ssh_retry_count=5 ssh_retry_delay=5s

# Configure a Proxmox remote source, without host key verification
$ plakar source add myProxmoxHypervisorRemote proxmox+backup://10.0.0.10 mode=remote conn_username=root conn_method=password conn_password=aSecureAndStrongPass ssh_insecure_ignore_host_key=true

# Backup VM / CT
$ plakar at /tmp/example backup -o vmid=101 @myProxmoxHypervisorSrc
$ plakar at /tmp/example backup -o pool=prod @myProxmoxHypervisorSrc
$ plakar at /tmp/example backup -o all @myProxmoxHypervisorSrc 
$ plakar at /tmp/example backup -o vmid=101 -o cleanup=false @myProxmoxHypervisorSrc 

# Backup VM / CT with custom cache and packfiles paths (redirected to a larger storage)
$ plakar at /tmp/example backup -cache /mnt/large-disk/cache -packfiles /mnt/large-disk/packfiles -o vmid=101 @myProxmoxHypervisorSrc

# Configure a Proxmox local destination
$ plakar destination add myProxmoxHypervisorLocal proxmox+backup://10.0.0.10 mode=local

# Configure a Proxmox remote destination (with password auth)
$ plakar destination add myProxmoxHypervisorRemote proxmox+backup://10.0.0.10 mode=remote conn_username=root conn_password=aSecureAndStrongPass  conn_method=password

# Configure a Proxmox remote destination (with identity auth)
$ plakar destination add myProxmoxHypervisorRemote proxmox+backup://10.0.0.10 mode=remote conn_username=root conn_identity_file=/path/to/something/pmx_id conn_method=identity

# Restore backup to destination
$ plakar at /tmp/example restore -to @myProxmoxHypervisorRemote <snapid>

# Restore one VM from a multi-VM snapshot by selecting its backup directory
$ plakar at /tmp/example restore -to @myProxmoxHypervisorRemote <snapid>:/backup/qemu/101_myvm
# Restore and restart after restore
$ plakar at /tmp/example restore -o start_on_restore=true -to @myProxmoxHypervisorRemote <snapid> 
# Restore over an existing VM (refused without this option, stops it first if running)
$ plakar at /tmp/example restore -o force_vm_restore=true -to @myProxmoxHypervisorRemote <snapid> 
# Restore to a different VMID and storage (new MAC addresses and SMBIOS UUID by default)
$ plakar at /tmp/example restore -o newid=201 -o storage=local-lvm -o pool=sharedpool -to @myProxmoxHypervisorRemote <snapid> 
# Restore to a different VMID but keep the source identity (MAC addresses, SMBIOS UUID)
$ plakar at /tmp/example restore -o newid=201 -o unique=false -to @myProxmoxHypervisorRemote <snapid> 
``` 

## Proxmox tools / commands used

This integration relies on Proxmox CLI tooling (`pvesh`, `vzdump`, `qmrestore`, `pct`).

Commands are executed locally when `mode=local`, and via SSH when `mode=remote`.

Backup (importer) commands:
- `pvesh get /version --output-format json`
- `pvesh get /cluster/resources --type vm --output-format json` (when `all`)
- `pvesh get /pools/<pool> --output-format json` (when `pool=...`)
- `vzdump <vmid> --dumpdir <dump_dir> --mode <snapshot|suspend|stop> --compress <0|1|lzo|gzip|zstd> [--node <node>]` (when `mode=local` and `mode=remote`)
- `cat -- /etc/pve/qemu-server/<vmid>.conf` (for QEMU sidecar config file)
- `cat -- /etc/pve/lxc/<vmid>.conf` (for LXC sidecar config file)
- `sh -c 'command -v <tool>'` and `test -d`/`test -w <dump_dir>` (preflight, before any VM is touched)
- `rm -f -- <dump_dir>/<archive>` and `rm -f -- <dump_dir>/<basename>.log` (when `cleanup=true`)

Restore (exporter) commands:
- `sh -c 'command -v <tool>'` and `test -d`/`test -w <dump_dir>` (preflight, before any upload)
- `df -Pk <dump_dir>` (free space check, before each archive is uploaded)
- `cat > <dump_dir>/<archive>` (write archive to Proxmox storage)
- `qm status <vmid>` / `pct status <vmid>` (check existence and running state)
- `pvesh get /pools/<pool> --output-format json` (only when a `_pool.conf` sidecar is present)
- `qmrestore <dump_dir>/<archive> <vmid> [--force] [--unique] [--storage <storage>] [--pool <pool>]` (QEMU)
- `pct restore <vmid> <dump_dir>/<archive> [--force] [--unique] [--storage <storage>] [--pool <pool>]` (LXC)
- `qm set <vmid> --smbios1 uuid=<new-uuid>,...` (QEMU only, when `unique` applies)
- `qm stop <vmid>` / `pct stop <vmid>` (when `-o force_vm_restore=true` and the VM/CT is running)
- `qm start <vmid>` / `pct start <vmid>` (only when `-o start_on_restore=true`)
- `rm -f -- <dump_dir>/<archive>` and `rm -f -- <dump_dir>/<basename>.log` (when `cleanup=true`)

## Technical / code overview 

### Backup Flow (Importer)

1. Read config and validate options (local/remote mode, SSH auth, compression, backup mode, node, etc.).
2. Preflight the target: required Proxmox tools are present, `dump_dir` exists and is writable.
3. Resolve VM/CT selection: `vmid`, `pool`, or `all`.
4. Retrieve the list via `pvesh`:
   `pvesh get /cluster/resources --type vm` or `pvesh get /pools/<pool>`.
   Resources the cluster reports with an `unknown` status (node unreachable) are skipped with a
   warning rather than attempted.
5. For each VM/CT, detect the type (`qemu` or `lxc`) via Proxmox inventory.
6. For each VM/CT, run `vzdump` to generate a dump file in `dump_dir`.
7. Read the dump file and send it to Plakar under `/backup/<type>/<vmid>_<vmname>/` (VM name is sanitized for path safety).
8. For QEMU and LXC, also export VM config files as sidecars:
   - QEMU: `/etc/pve/qemu-server/<vmid>.conf` as `/backup/qemu/<vmid>_<vmname>/<dump>_qemu.conf`
   - LXC: `/etc/pve/lxc/<vmid>.conf` as `/backup/lxc/<vmid>_<vmname>/<dump>_lxc.conf`
9. If VM/CT belongs to a pool, export pool membership as `/backup/<type>/<vmid>_<vmname>/<dump>_pool.conf` (content is the pool name).
10. `cleanup` option: the generated dump file, and the `.log` vzdump writes next to it, are removed
    from `dump_dir` (enabled by default). Removal happens once Plakar is done reading the archive,
    and on failure paths too, so a job that keeps failing does not fill up `dump_dir`.

### Partial failures

A VM/CT that cannot be dumped (locked by a migration, a snapshot in progress, a missing disk, ...)
does not cancel the run. The failure is reported as a failed entry for that VM/CT, a warning is
printed on stderr, and the remaining selection is backed up normally. The backup only fails as a
whole when *every* selected VM/CT failed.

### Restore Flow (Exporter)

1. Read snapshot files (dumps and optional sidecars).
2. Collect sidecar configs (`_qemu.conf`, `_lxc.conf`) and map them to their dump names.
3. For each dump file, parse the restore target from the filename (type + vmid), then write the dump into `dump_dir`.
4. Check target existence and runtime state using `qm/pct status`.
5. If the VM/CT exists, the restore is refused unless `-o force_vm_restore=true`; with that option
   a running VM/CT is stopped first, then overwritten.
6. If VM/CT does not exist, restore dump directly.
7. Restore options from `plakar restore -o` are applied:
   - `start_on_restore=true|false` (`false` by default): start VM/CT after successful restore.
   - `force_vm_restore=true|false` (`false` by default): allow overwriting an existing VM/CT,
     stopping it first if it is running.
   - `unique=true|false` (defaults to `true` when `newid` is set): random MAC addresses, plus a new
     SMBIOS UUID for QEMU.
   - `storage=<name>`: force restore storage,
   - `pool=<name>`: force restore pool (validated on target),
   - `newid=<id>`: restore to another VMID.
8. Storage/pool precedence:
   - user-specified `storage` and `pool` override sidecar-derived hints when present.
   - if target VMID does not exist and no override is set, storage and pool are read from matching
     sidecars when available. A pool that no longer exists on the target is reported as a warning
     and dropped, instead of failing the restore.
9. `cleanup` option: remove the temporary dump from `dump_dir`. It runs whether the restore
   succeeded or not, so a failing restore does not leave multi-gigabyte archives behind. Set
   `cleanup=false` to keep them for inspection.

### Remote Mode and SSH Notes

Remote mode exists to avoid installing extra binaries on the hypervisor and to centralize multiple Proxmox backups from a single "backup relay".

A single SSH connection is kept open and reused for the whole backup/restore job (vzdump execution, archive read/write, config/pool sidecar reads, cleanup, ...). On a long-running job (e.g. a large LXC/QEMU dump taking tens of minutes), that connection can become stale (idle timeouts on a NAT/firewall/VPN sitting between Plakar and Proxmox, a heavily loaded Proxmox node, a flaky link, ...), which typically surfaces as an SSH channel-open failure such as:

To make this resilient, every SSH action (opening a channel to run a command, read/write a file, ...) is retried on failure: the SSH connection is re-dialed and the action is attempted again, up to `ssh_retry_count` times (default `3`), waiting `ssh_retry_delay` between attempts (default `2s`). Set `ssh_retry_count=0` to disable retries and fail immediately, as before.
