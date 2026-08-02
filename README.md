# hypervm-mcp

Manage Hyper-V from Claude Code without a UAC prompt on every session.

[![CI](https://github.com/heavycaffeiner/hypervm-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/heavycaffeiner/hypervm-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## Install

```powershell
irm https://raw.githubusercontent.com/heavycaffeiner/hypervm-mcp/main/install.ps1 | iex
```

One UAC prompt, and that is the last one. The installer downloads the latest
release, verifies its checksum, installs the service, and registers the MCP
server with Claude Code if it finds it.

<details>
<summary>Other ways in</summary>

With Go:

```powershell
go install github.com/heavycaffeiner/hypervm-mcp/cmd/hypervm-mcp@latest
hypervm-mcp service install
claude mcp add hypervm-mcp -s user -- "$env:ProgramData\hypervm-mcp\bin\hypervm-mcp.exe" bridge
```

From a release, by hand: download `hypervm-mcp.exe`, check it against
`hypervm-mcp.exe.sha256`, then run `hypervm-mcp.exe service install`.

Or add it to a workspace's `.mcp.json` instead of registering it globally:

```json
{
  "mcpServers": {
    "hypervm-mcp": {
      "command": "C:\\ProgramData\\hypervm-mcp\\bin\\hypervm-mcp.exe",
      "args": ["bridge"]
    }
  }
}
```
</details>

Check it worked:

```powershell
hypervm-mcp doctor
```

**Requires** Windows 10/11 Pro or Server with Hyper-V enabled, and PowerShell 5.1.

## Why this exists

Hyper-V's management API requires administrator rights. MCP clients run
unprivileged and start a fresh server process per session, so a server built
with a `requireAdministrator` manifest raises a UAC prompt every time — which
stalls any automated workflow behind a mouse click.

This moves the privilege boundary from *every process launch* to *one install*. A
LocalSystem service holds the Hyper-V rights, and a tiny unprivileged bridge
relays MCP traffic to it over a named pipe whose ACL names exactly one account.

```
[Claude Code] --stdio--> [hypervm-mcp bridge] --pipe--> [hypervm-mcp service] --> Hyper-V
  unprivileged             unprivileged                  LocalSystem
```

The bridge does not parse MCP; it copies bytes. All protocol handling lives in
the service, so adding tools never means redeploying the bridge.

## What you can do with it

Ask Claude Code things like:

- *"Create a Rocky Linux VM with 4 GB of RAM on D:, boot it from this ISO."*
- *"The web server in that VM listens on its own 127.0.0.1. Get me to it."*
- *"Expose it to my tailnet at an HTTPS name."*
- *"Give me four 512 MB disks on that VM so I can build a RAID array."*
- *"Snapshot it, upgrade the kernel, and roll back if it breaks."*
- *"Put these two VMs on a private network with the host, and tell me the addresses."*

## Tools

<details open>
<summary><b>Lifecycle</b></summary>

| Tool | |
|---|---|
| `list_vms` `get_vm` | Inventory and full detail |
| `start_vm` `stop_vm` `restart_vm` | Power control; graceful unless `force` |
| `suspend_vm` `resume_vm` | Save to disk or pause in memory |
| `wait_for_guest_ip` | Wait until the guest is actually reachable |
</details>

<details>
<summary><b>Checkpoints</b></summary>

| Tool | |
|---|---|
| `create_checkpoint` `list_checkpoints` | Snapshot and inspect |
| `apply_checkpoint` | Revert, discarding everything since |
| `delete_checkpoint` | Remove one and wait for its disk to merge |
</details>

<details>
<summary><b>Provisioning and storage</b></summary>

| Tool | |
|---|---|
| `create_vm` `delete_vm` | Independent paths for config, disk, checkpoints, paging |
| `create_vhd` `attach_vhd` `detach_vhd` | Disks sized in MB, placed on a controller port you choose |
| `add_scsi_controller` | A second controller, past 64 disks |
| `attach_iso` `eject_dvd` | Installation media in and out |
| `create_seed_disk` | An answer-file disk for an unattended install |
| `create_vm_from_template` | Clone from a golden image in seconds |
| `export_vm` `import_vm` | Produce an image, or move a VM's fixed config path |
| `get_vhd_info` `resize_vhd` `convert_vhd` | Inspect and reshape |
| `get_host_storage_paths` `set_host_storage_paths` | Where things land by default |
</details>

<details>
<summary><b>Networking</b></summary>

| Tool | |
|---|---|
| `list_switches` `create_switch` `delete_switch` | Virtual switches |
| `set_switch_host_address` `set_switch_nat` | The host's side of an Internal switch |
| `set_vm_network` | Switch, static MAC, VLAN, MAC spoofing, extra adapters |
| `set_guest_static_ip` | A fixed address inside the guest |
| `list_physical_adapters` `preflight_external_switch` | Before touching a physical NIC |
| `diagnose_vm_network` | What can reach this VM, and what cannot |
</details>

<details>
<summary><b>Guest access</b></summary>

| Tool | |
|---|---|
| `ssh_exec` `ssh_info` `ssh_forget_host_key` | Commands over SSH, host keys pinned per VM |
| `guest_invoke_command` `guest_copy_file` | Over the VMBus, with no guest network at all |
| `send_vm_key` | Press keys at the console, before there is a guest to talk to |
| `get_guest_network` | Adapters and reported addresses |
| `open_tunnel` `list_tunnels` `close_tunnel` | Forward a host port into a VM |
| `tailscale_serve` `tailnet_status` | HTTPS at your MagicDNS name |
| `doctor` | Check everything and say what to fix |
</details>

## Three ways to reach a service inside a VM

Picking the wrong one wastes an afternoon, so the tools say which applies.

**Connect directly.** The host dialling the guest's own address binds no host
port, so the host's own listeners are irrelevant. Guest SMB on 445 works this way
over the Default Switch — no tunnel, no External switch.

**A tunnel** forwards a host port in. Two data paths: `direct` dials the guest's
address, and `ssh` forwards through the guest's sshd — the only way to reach a
service bound to the guest's own `127.0.0.1`. `bind_scope` decides who can
connect: this host, your tailnet (with a firewall rule scoped to those addresses,
created and removed with the tunnel), everything, or one address.

Tunnels live in the service, not your session. They survive the conversation
ending, are restored when the service restarts, and re-resolve the guest address
when a VM reboots onto a new IP.

**An External switch** puts the guest on the physical LAN with its own address.
Only needed when *other machines* must reach the guest, or it must see broadcast
traffic.

> [!CAUTION]
> **Creating an External switch is the one thing here that can break your
> machine.** Hyper-V rebinds the physical adapter, dropping host networking for
> several seconds — and when a static address fails to migrate, leaving the host
> with no working address, recoverable only from the console. Nothing else this
> server does touches host networking.
>
> `create_switch` refuses one until `confirm_disruption` is set, and the refusal
> reports which risks apply to *your* host: whether that adapter is your only
> uplink, whether its address is static, whether it is wireless and so will not
> truly bridge anyway, and what happens to Tailscale.
> `preflight_external_switch` shows the same report without changing anything.
>
> **This path is unverified** — see [What is verified](#what-is-verified).

## Things that will bite you

**Guest IPs need an agent inside the guest.** Hyper-V does not discover them; it
reads what an agent reports. A minimal Linux install ships without one, so a
healthy VM with a working DHCP lease reports nothing and `wait_for_guest_ip`
times out. Install `hyperv-daemons` and reboot. Until then, `ssh_exec`,
`open_tunnel` and `diagnose_vm_network` all accept the address directly.

**An Internal switch has no DHCP.** The Default Switch arrives with an address,
NAT and DHCP arranged, which hides that all three are normally your job. One you
create has none: use `set_switch_host_address` for the host, `set_guest_static_ip`
for each guest, and `set_switch_nat` if they need the internet.

**A template must stay powered off while its clones run.** A running VM opens its
disk exclusively, and a differencing disk cannot read a parent held that way. The
clone fails to start with a file-in-use error that never mentions differencing
disks.

**Paths are opened by the service, not by you.** It runs as LocalSystem with its
own logon session, so mapped drive letters do not exist for it — use UNC, and
grant `HOSTNAME$` access to the share. Every path is checked before anything is
created.

## Credentials

Guest credentials are stored by the service, encrypted with machine-scope DPAPI,
so they never travel through a conversation:

```powershell
hypervm-mcp cred set --vm Dev-Ubuntu --user dev --ssh-key ~\.ssh\id_ed25519
hypervm-mcp cred list     # what is on file, never the secrets
```

The CLI hands them over the pipe rather than writing the file, because the two
ends run as different accounts: the CLI is you, and only the service can write
its own data directory.

SSH host keys are pinned per VM name on first connect. A later mismatch fails
until you pass `trust_new_key` — the expected path after rebuilding a VM.
Keying on the VM name rather than the address is deliberate: an address-keyed
store would see a new host after every reboot and pin nothing.

## CLI

```
hypervm-mcp bridge                  Relay MCP traffic (run by MCP clients)
hypervm-mcp service install         Install and start (one UAC prompt)
hypervm-mcp service uninstall       Remove; --purge also deletes stored data
hypervm-mcp service start | stop | status
hypervm-mcp cred set | list | delete
hypervm-mcp tunnel list
hypervm-mcp doctor                  Check the setup and report what to fix
hypervm-mcp version
```

## Where things live

```
%ProgramData%\hypervm-mcp\
  bin\hypervm-mcp.exe    the binary the service runs
  config.json            pipe name, allowed SID, PowerShell path, limits
  credentials.dat        guest credentials, DPAPI machine scope
  known_hosts.json       pinned SSH host keys, by VM name
  tunnels.json           tunnel definitions, reopened on restart
  logs\service.log       warnings and errors also go to the event log
```

The directory's ACL is explicit and non-inherited: LocalSystem and Administrators
get full control, the installing user read-only. The binary lives here rather
than in your build tree because anything writable by a non-admin that a
LocalSystem service executes is a privilege escalation.

## Troubleshooting

Run `hypervm-mcp doctor` first — it checks Hyper-V, the pipe, storage paths,
switches, credentials, Tailscale and every open tunnel, and says what to do.

| Symptom | |
|---|---|
| *"the hypervm-mcp service is not running"* | `hypervm-mcp service start` |
| *"access to \\\\.\\pipe\\hypervm-mcp was denied"* | The pipe accepts only the account it was installed for. Reinstall as that account. |
| `ACCESS_DENIED` *"Hyper-V refused the caller"* | PowerShell ran without Hyper-V rights; check the service runs as LocalSystem. |
| `GUEST_IP_UNAVAILABLE` | Usually the missing guest agent above. |
| `SSH_HOST_KEY_MISMATCH` | Expected after rebuilding a VM. Pass `trust_new_key`, or `ssh_forget_host_key`. |
| `PORT_IN_USE` on 445, 3389, 5985 | Windows holds those; they cannot be tunnelled. |

## Design notes

**Arguments never enter script text.** Every value is sent as JSON on stdin and
read back as `$P` inside the PowerShell script. A VM named
`Dev"; Remove-Item C:\ -Recurse` is a string, not code. There is a test for
exactly that.

**Success is a field, not an exit code.** PowerShell leaves the exit code at 0
for non-terminating errors, so scripts are wrapped in an envelope reporting `ok`
explicitly, with `$ErrorActionPreference = 'Stop'` promoting everything to
terminating.

**Projections build hashtables, never `Select-Object` calculated properties.**
Select-Object flattens its expression's output: an array of one becomes a scalar
and an empty one becomes `{}`. Both decode wrongly, and the one-element case is
the common one — a VM usually has exactly one IP address.

**Nothing depends on the host's display language.** Hyper-V localizes its
messages, so "VM not found" arrives in Korean on a Korean Windows. Outcomes are
decided by error *category* and by cmdlet id, both locale-independent, with an
`en-US` pin so anything reaching the text-matching fallback is English.

## Developing against an installed copy

If you already use hypervm-mcp for real work, you do not want a development build
restarting that service, reading its credentials, or answering on its pipe. Build
with an instance name and it takes a separate identity throughout:

```powershell
go build -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev" `
  -o bin\hypervm-mcp-dev.exe .\cmd\hypervm-mcp
.\bin\hypervm-mcp-dev.exe service install
```

| | release build | `instance=dev` |
|---|---|---|
| Service | `hypervm-mcp` | `hypervm-mcp-dev` |
| Pipe | `\\.\pipe\hypervm-mcp` | `\\.\pipe\hypervm-mcp-dev` |
| Data directory | `%ProgramData%\hypervm-mcp` | `%ProgramData%\hypervm-mcp-dev` |
| Event log source | `hypervm-mcp` | `hypervm-mcp-dev` |
| Firewall rules and NATs | `hypervm-mcp-*` | `hypervm-mcp-dev-*` |
| MCP server name | `hypervm-mcp` | `hypervm-mcp-dev` |

Release builds pass no flag, so their names are unchanged. The two services run
side by side; this repository's `.mcp.json` points at the dev one.

Credentials and pinned host keys do **not** carry over — the dev instance starts
with an empty store, which is the point.

**Hyper-V itself is not isolated.** Both instances drive the same hypervisor and
see the same virtual machines. Keeping their VM names apart is still up to you.

## Tests

```powershell
go test ./...
```

Everything touching real infrastructure is opt-in, because it costs a VM.

```powershell
$env:HYPERVM_E2E = "$env:ProgramData\hypervm-mcp\bin\hypervm-mcp.exe"
go test ./internal/e2e -count=1 -v          # against the installed service

$env:HYPERVM_E2E_ROCKY = "1"
$env:HYPERVM_E2E_INSTALL = "1"              # rebuilds the VM; separately gated
go test ./internal/e2e -run Rocky -count=1 -v -timeout 90m
```

`TestRockyProvision` installs Rocky Linux 10 unattended from a kickstart on an
`OEMDRV` seed disk, which Anaconda finds on its own with no boot parameter and no
rebuilt ISO. The rest build on that guest: nginx behind an SSH tunnel, SMB from
the host, a RAID array across attached disks, a private three-way network.

## What is verified

Everything here is implemented. This says which parts have been run against real
hardware, because on a public server that is a different claim.

**Exercised end to end**, against Rocky Linux 10 on Windows 11:

| | |
|---|---|
| Unattended install | Kickstart on an `OEMDRV` seed disk, 3.5 minutes, no console input |
| SSH tunnel | nginx bound to the guest's `127.0.0.1`, with a `direct` tunnel proven to fail first |
| Tailnet tunnel | Both tailnet addresses; firewall rule created and removed, checked against Windows |
| `tailscale serve` | Served at the MagicDNS name over HTTPS |
| Guest SMB from the host | Read and write over the Default Switch, with the 445 tunnel refused alongside |
| Checkpoints | Snapshot, change the guest, revert, confirm the change is gone, merge |
| Golden-image clone | A 32 GB VM in 6.9 seconds; deleting the clone leaves the image intact |
| Disks and RAID | Four 512 MB fixed disks at chosen ports, assembled into RAID5 by the guest |
| ISO mount | Attached to a running VM, mounted and read by the guest |
| Private network | Internal switch, static IPs, host and two guests all reaching each other, plus NAT |
| Network diagnosis | Port probes, and telling "no address reported" apart from "unreachable" |
| External switch guard | The refusal and its personalised report — but not creation |

**Exercised end to end**, against Windows Server 2022 (Server Core):

| | |
|---|---|
| Unattended install | `autounattend.xml` on its own small ISO, no console input |
| `guest_invoke_command` | PowerShell Direct over the VMBus, before the guest had any network service |
| SSH bootstrap | OpenSSH installed, started, keyed and firewalled entirely over the VMBus, then reached over TCP |
| `guest_copy_file` | Host to guest over the VMBus; Windows carries the component in the box |

The answer file deliberately does not install OpenSSH. Putting sshd there would
have made the bootstrap test prove nothing.

**Implemented but never run:**

- **Creating or deleting an External switch.** Running it means disconnecting a
  working machine, so it is left for a deliberate session with console access.
  Its guards and preflight are tested; the creation is not.
- **`export_vm` / `import_vm` / `resize_vhd` / `convert_vhd`.**

**Known environment limits, not defects:**

- `guest_copy_file` cannot work on Linux 6.10 or later, which removed the
  `/dev/vmbus/hv_fcopy` device `hypervfcopyd` attaches to. The daemon still
  reports itself active. The error says so and points at `ssh_exec`.

## License

MIT. See [LICENSE](LICENSE).
