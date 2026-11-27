# Proxmox Integration

Back up Proxmox VE virtual machines (QEMU) and containers (LXC) with Plakar, restore them
to a Proxmox node, and list them as an inventory.

## Install

1. Build and install the plugin on the machine running Plakar:
   ```bash
   $ make install        # builds proxmox_<version>_<os>_<arch>.ptar and runs plakar pkg add on it
   ```
2. Install [plakar-pve-provider](https://github.com/gillesdubois/plakar-pve-provider)
   on every node and declare its `plakar` storage. It needs Proxmox VE 8.4 or later. LXC containers
   don't need it. See project's readme.
3. Plakar either runs on the node itself (`mode=local`), or connects to it over SSH as `root`
   (`mode=remote`, with `AllowStreamLocalForwarding` left at its OpenSSH default).

## Quick start

```bash
# Create the Plakar repository that will hold the backups (once)
$ plakar at /var/backups create

# The node to back up, over SSH with a key
$ plakar source add pve proxmox+backup://10.0.0.10 mode=remote conn_method=identity conn_username=root conn_identity_file=~/.ssh/pve_id

# Back up VM 101
$ plakar at /var/backups backup -o vmid=101 @pve

# List the snapshots and note the ID of the one to restore
$ plakar at /var/backups ls

# The node to restore to, then restore
$ plakar destination add pve-restore proxmox+backup://10.0.0.10 mode=remote conn_method=identity conn_username=root conn_identity_file=~/.ssh/pve_id
$ plakar at /var/backups restore -o vmid=101 -to @pve-restore <snapid>
```

If a VM or CT with the same VMID already exists on the target node, the restore is refused: remove
it first, or pass `-o force_vm_restore=true` (see [Restoring](#restoring)).

With Plakar installed on the node, use `mode=local` and no `conn_*` options. In `mode=local`,
Plakar must run as `root` on the node.

## Choosing what to back up

Pass exactly one of these to `plakar backup`:

| Option | Backs up |
|---|---|
| `-o vmid=<id>` | one VM or CT |
| `-o pool=<name>` | every VM and CT of the pool |
| `-o all=true` | every VM and CT |

Only guests running on the node Plakar connects to are backed up. In a cluster, back up each node
from its own source. Guests of a pool that run on another node are skipped with a warning.

## Restoring

A restore brings back one VM or CT, chosen with `-o vmid=<id>` (required), whatever the snapshot
holds. It never overwrites an existing VM or CT unless you ask for it.

```bash
# VM 101, out of a vmid, pool or all snapshot
$ plakar at /var/backups restore -o vmid=101 -to @pve-restore <snapid>

# Restore and start it
$ plakar at /var/backups restore -o vmid=101 -o start_on_restore=true -to @pve-restore <snapid>

# Restore under another VMID, on another storage
$ plakar at /var/backups restore -o vmid=101 -o newid=201 -o storage=local-lvm -to @pve-restore <snapid>

# Restore a copy while the original still runs on the same network: new MAC addresses
$ plakar at /var/backups restore -o vmid=101 -o newid=201 -o unique=true -to @pve-restore <snapid>

# Replace the existing guest (destructive, see the warning below)
$ plakar at /var/backups restore -o vmid=101 -o force_vm_restore=true -to @pve-restore <snapid>
```

> [!WARNING]
> `force_vm_restore=true` stops the target VM/CT, then **destroys its current disks and
> configuration** and replaces them with the backup. Everything written since the backup is lost.
> It applies to the target VMID, so combined with `newid` it destroys whatever guest owns that ID.

| Option | Default | Effect |
|---|---|---|
| `vmid` | required | the VM/CT to restore from the snapshot |
| `force_vm_restore` | `false` | replace an existing VM/CT: stops it, destroys its disks and config (see the warning above) |
| `start_on_restore` | `false` | start the VM/CT once restored |
| `newid` | source VMID | restore under another VMID |
| `unique` | `true` with `newid`, else `false` | new MAC addresses, and a new SMBIOS UUID for QEMU; set it when the original guest still runs on the same network, to avoid MAC/IP conflicts |
| `storage` | storage of the boot disk (QEMU) or `rootfs` (LXC) | storage for **all** disks: a VM whose disks were on several storages comes back with all of them on one |
| `pool` | the original one | pool to put the VM/CT in (it must exist) |

If the pool recorded in the backup no longer exists, the guest is restored outside any pool, with a
warning.

## Configuration reference

Connection:

| Option | Required | Description |
|---|---|---|
| `mode` | yes | `local` (Plakar on the node) or `remote` (over SSH) |
| `conn_method` | `remote` | `password` or `identity` |
| `conn_username` | `remote` | SSH user, must be `root` |
| `conn_password` | `password` | SSH password |
| `conn_identity_file` | `identity` | path to the private key, or use `conn_identity_key` |
| `conn_identity_key` | `identity` | private key content, or use `conn_identity_file` |

Advanced:

| Option | Default | Used by | Description |
|---|---|---|---|
| `backup_mode` | `snapshot` | backup | vzdump mode: `snapshot`, `suspend` or `stop` (stops the guest during the backup) |
| `fleecing_storage` | `/etc/vzdump.conf`, else the VM's boot disk storage | backup | storage for the fleecing image of QEMU VMs (e.g. `local-lvm`), which Proxmox requires for provider backups; prefer a thin storage that supports discard |
| `node` | the connected node | backup | safety check: refuse to run if the connected host is not this node |
| `dump_dir` | `/var/lib/vz/dump` | restore | where archives and disk images are uploaded before the restore; they are always removed afterwards |
| `ssh_retry_count` | `3` | `remote` | retries when an SSH channel fails to open, `0` to disable |
| `ssh_retry_delay` | `2s` | `remote` | delay between SSH retries |

Dumps are never compressed by Proxmox: Plakar deduplicates and compresses them itself.

## Inventory

The `proxmox-inventory` connector lists the QEMU VMs and LXC containers of a Proxmox host or
cluster through the Proxmox API, so each can be backed up with the importer. Templates are not
listed. Guests on an unreachable node are, with the `status=unknown` tag.

It is meant for the Plakar control plane: add the inventory there, and each guest shows up as a
resource to protect, with the importer options below already filled in.

It authenticates with an API token. The `PVEAuditor` role on `/` is enough:

```bash
$ pveum user add plakar@pve
$ pveum acl modify / --users plakar@pve --roles PVEAuditor
$ pveum user token add plakar@pve inventory --privsep 0
```

| Option | Required | Description |
|---|---|---|
| `location` | yes | API address: `https://host[:port][/prefix]` (port `443` by default, prefix for a reverse proxy), or `host[:port]` (port `8006` by default) |
| `token_id` | yes | token ID, e.g. `plakar@pve!inventory` |
| `token_secret` | yes | token secret, shown once by `pveum user token add` |
| `tls_insecure` | no | `true` to skip certificate verification, for the self-signed certificate Proxmox ships with |

Each guest has:

- the URN `urn:proxmox:<cluster>:<qemu|lxc>:<vmid>`, where `<cluster>` is the node name on a
  standalone host. Joining a node to a cluster, or renaming the cluster, changes the URNs;
- its node's IP as endpoint, which becomes the importer's `location`, since a backup must run on
  the guest's node. It is the cluster network IP, which may not be reachable from Plakar. Without
  an IP, the node name is used instead;
- the `vmid` and `node` importer options, filled in when the control plane creates the guest's
  importer. `mode` and the SSH options are still to set;
- the guest's Proxmox tags, plus `pool=`, `type=` and `status=`.

Proxmox tags cannot hold `:`, so the Plakar environment and data class tags are written with
dashes and translated:

| Proxmox tag | Plakar tag |
|---|---|
| `plakar-environment-<name>`, `plakar-env-<name>` | `plakar:environment:<name>` |
| `plakar-data-class-<name>`, `plakar-dataclass-<name>`, `plakar-class-<name>` | `plakar:data-class:<name>` |

## Snapshot layout

```
/lxc/<vmid>_<name>/vzdump-lxc-<vmid>-<time>.tar            container dump
/lxc/<vmid>_<name>/vzdump-lxc-<vmid>-<time>.tar_lxc.conf   its config
/lxc/<vmid>_<name>/vzdump-lxc-<vmid>-<time>.tar_pool.conf  its pool, if any
/qemu/<vmid>_<name>/manifest.json              VM config, firewall, pool, disk sizes
/qemu/<vmid>_<name>/vm-<device>.raw             one raw image per disk, e.g. vm-drive-scsi0.raw
```

`<time>` is the backup start time on the machine running Plakar, as `YYYY_MM_DD-HH_MM_SS`.

## Disk space

- A restore needs room in `dump_dir` for all disks of a QEMU VM, or the whole archive of a CT, on the node.
- Plakar's own cache and packfiles can fill the machine running Plakar on large VMs. Move them with
  `plakar backup -cache <dir> -packfiles <dir>`.

## Further reading

- [COMMANDS.md](COMMANDS.md): every command run and file touched on the node.
- [DESIGN.md](DESIGN.md): how backups, restores and the inventory work inside.
