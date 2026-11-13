# Accretion

Accretion is a declarative VM engine. You write a manifest saying which
machines should exist, `accrete plan` lines that up against what was built and
what the hypervisor reports, and `accrete apply` builds, powers and removes
machines until they match. Each machine is tied to its record by the SMBIOS
UUID it was given when it was built, not by its name or VMID. So a guest that
somebody else put at the same VMID is never mistaken for yours.

v0.1 has one provider, Proxmox VE. It is the same engine that Sybil
Continuum's Environments use: the planner, the manifest and the Proxmox code
are shared, and this module never imports Sybil (`boundary_test.go`).

## Install

You need Go 1.26.1 or later.

```sh
go install github.com/hazame-hub/accretion/cmd/accrete@latest
accrete plan -h
```

Or from a clone of this repository:

```sh
go build -o accrete ./cmd/accrete
./accrete plan -h
```

The binary is self-contained. Put it anywhere on your `PATH`.

## Quickstart on Proxmox (5 minutes)

You need:
- a Proxmox VE node;
- a cloud-init template with the QEMU guest agent installed, for example a
  Debian or Rocky cloud image turned into a template (`qm template <vmid>`);
- a storage that accepts ISO content (`local` does by default);
- a DHCP server on the bridge.

1. Create an API token. In the Proxmox UI, go to Datacenter → Permissions →
   API Tokens. Unless a resource declares `pool`, accrete puts new machines in
   no pool, so grant these on `/` (or on `/vms`, the template, the storages,
   the node and the bridge, as Proxmox's permission paths allow). With
   `pool: <name>`, the machines' privileges go on `/pool/<name>` instead, and
   every other path gets a role of its own holding only what is used there
   (the template only `VM.Audit` and `VM.Clone`, the disk storage only
   `Datastore.AllocateSpace` and `Datastore.Audit`), so the token can neither
   touch guests outside the pool, change the template, nor delete volumes it
   did not make. `accrete permissions` prints these roles:
   - on the machines (`/vms`): `VM.Allocate`, `VM.Clone`, `VM.Audit`,
     `VM.PowerMgmt`, `VM.Config.CPU`, `VM.Config.Memory`, `VM.Config.Disk`,
     `VM.Config.Network`, `VM.Config.Options`, `VM.Config.CDROM`,
     `VM.Config.Cloudinit` (Proxmox checks it when the seed goes on a clone of
     a template that has a cloud-init drive) and `VM.Config.HWType`;
   - on the disk storage: `Datastore.Audit` and `Datastore.AllocateSpace`;
   - on the seed storage (`snippet_storage`): `Datastore.Audit`,
     `Datastore.AllocateTemplate` (to upload the seed ISO) and
     `Datastore.Allocate` (to remove it);
   - on the node: `Sys.Audit`;
   - on the bridge (Proxmox VE 8 and 9): `SDN.Use`, on
     `/sdn/zones/localnetwork/<bridge>`, or on `/sdn/zones/<zone>/<network>`
     for a network the manifest declares;
   - only while a declared network has to be created: `SDN.Allocate` and
     `SDN.Audit` on `/sdn`, and `Sys.Modify` on the nodes (to reload their
     network so the bridge comes up).

   Optional: `VM.GuestAgent.Audit` (Proxmox VE 9) or `VM.Monitor` (VE 8) on
   the machines lets `plan` show the guest agent's hostname and addresses;
   without it they are left out, and nothing else changes. Optional too:
   `SDN.Audit` on `/sdn`, without which Proxmox lists the SDN as empty and
   every `plan` warns that its listing is incomplete.

   No built-in role covers all of these. `accrete permissions -f env.yaml`
   (step 3 first) prints the exact role your manifest needs, the paths to
   grant it on, and the UI steps and `pveum` commands to do it, then checks
   your token against it. A token that misses one is refused with the
   privilege and path named.
2. Put the token in an environment variable. It is never written into the
   manifest or the state:

   ```sh
   export ACCRETE_PROXMOX_TOKEN='ops@pve!accrete=<secret>'
   ```

3. Write `env.yaml`, setting the endpoint, node and template to your own:

   ```yaml
   apiVersion: accretion/v1alpha1
   kind: Environment
   metadata:
     name: lab
   spec:
     provider:
       type: proxmox
       endpoint: https://pve.example.lan:8006
       verify_tls: false          # a self-signed certificate; keep true when it is trusted
       token_env: ACCRETE_PROXMOX_TOKEN
     resources:
       - name: web
         count: 2
         blueprint: provision-proxmox-vm
         inputs:
           pve_node: pve
           template_vmid: "9000"
           hostname: web-{{instance.number}}
           cores: 2
           memory_mb: 2048
           bootstrap:
             users:
               - name: ops
                 ssh_keys: ["ssh-ed25519 AAAA… you@laptop"]   # your own public key
                 sudo: true
   ```

4. Plan, apply, look, destroy:

   ```sh
   accrete plan -f env.yaml        # 2 to create; nothing is sent but GETs
   accrete apply -f env.yaml       # shows the plan, asks for "yes", builds web-1 and web-2
   accrete plan -f env.yaml        # 2 unchanged
   accrete destroy -f env.yaml     # asks, then stops and deletes both, and their seeds
   ```

The state is kept in `accrete.state.json` beside the manifest, or wherever
`--state` points. Keep it: it is how accrete knows which machines are its own.
It holds no endpoint and no credential.

## Commands

| Command | What it does |
| --- | --- |
| `accrete plan -f env.yaml [--state F] [--terraform-state P] [-o text\|json] [--offline]` | Prints what apply would do. Sends GET requests only and writes nothing. `--offline` asks the provider nothing. |
| `accrete apply -f env.yaml [--state F] [--terraform-state P] [--auto-approve] [--forget-gone]` | Prints the plan, asks, then acts on it one machine at a time. |
| `accrete destroy -f env.yaml [--state F] [--terraform-state P] [--auto-approve] [--forget-gone]` | Removes every machine the state records for this environment, if the provider confirms it is still the machine that was built. |
| `accrete init --from-cluster --provider T --endpoint URL --token-env V [--name N] [--select RE] [--template ID] [-f F]` | Writes a manifest and a state for the VMs the provider runs now, adopted under their current identity, then plans them. GET requests only; never overwrites a file. |
| `accrete verify --terraform-state PATH (-f env.yaml \| --provider T --endpoint URL --token-env V) [--select RE] [--terraform-power] [-o text\|json]` | Checks what a Terraform state records against what the provider reports, resource by resource, and lists the machines the state does not record. GET requests only; writes no state of either tool. Exit 1 when something differs or is unknown. |
| `accrete import --from-terraform STATE --provider T --endpoint URL --token-env V [--select RE] [--template ID]` | Adopts the VMs a Terraform state records and writes `terraform-handover.tf` (removed blocks) for Terraform to forget them without destroying them. GET requests only. |
| `accrete export --terraform -f env.yaml [--select RE]` / `accrete export --retire --terraform-state P -f env.yaml` | Writes import blocks for Terraform to take machines over; then retires accrete's records of those Terraform now records. |
| `accrete image fedora-coreos [--stream stable\|testing\|next] [--storage S] [--arch A]` | Prints the `image`, `image_url` and `image_sha256` inputs pinning `image-proxmox-vm` to the stream's current Fedora CoreOS proxmoxve image. Asks Fedora's stream metadata only; changes nothing. |
| `accrete bundle -f install.yaml [-o DIR]` | Writes one kickstart for every machine a NetworkInstall file declares; each machine picks its own profile when it boots. See [Network installs](#network-installs-and-shared-answer-files). |
| `accrete pxe -f install.yaml [-o DIR]` | Writes an iPXE gate, the kickstart for the armed machines and a proxy-DHCP config. Contacts nothing. |
| `accrete permissions -f env.yaml [-o text\|json] [--offline] [--role NAME]` | Prints the one role the token needs to apply this manifest (template, machines, disk and seed storages, node, bridge), the paths to grant it on and how, and what the token lacks on each now. GET requests only. Exit 1 when a required privilege is missing, 3 when a path could not be checked. |

Every command also takes `--timeout` (default 2 minutes for plan, 1 hour for
apply and destroy): how long the whole command may run.

**What apply does with each plan line:**
- **create:** clone the template, seed cloud-init, start the machine, confirm it, record it. For
  `install-proxmox-vm`: create an empty machine, give it its answer disc, start the installer,
  wait for its power-off, remove the discs, start it from its disk, accept it on its agent's
  hostname, record it (see below). For `image-proxmox-vm`: create the machine with its disk
  copied from a disk image, seed it, start it, wait for its first boot to power it off, start
  it again, confirm it, record it (see below).
  For a clone with more than one card or a fixed address, the seed's network config
  names each card by the MAC Proxmox gave it at clone time, so each card gets
  its own addressing whatever the guest calls its interfaces. Two machines
  declared with the same fixed address are a plan error.
- **create** of a network (`spec.networks`, a network the cluster lacks):
  define what is missing of its simple zone (with the PVE IPAM, and dnsmasq
  DHCP when the network has a range), its vnet and its subnet; apply the
  cluster's SDN; reload each online node's network; wait until the vnet is up
  on every node. Networks come first. A machine on a network created in the
  same apply is created after it (`create web/0 (on web, once created)`), and
  is not started when its network failed. A rerun finishes a half-made
  network. Refused before anything is sent: a zone that exists as another
  type or with the other DHCP setting (DHCP is a zone setting), SDN edits
  somebody else left unapplied (the cluster apply would publish them), and a
  node with network edits staged (its reload would apply them).
- **delete** (a machine the manifest no longer declares): stop it, delete it, remove its seed, retire its record.
- **change** that is a power difference: power the machine on or off.
- **change** of cores, memory or disk size (the manifest changed, or the
  machine was changed outside accrete, or such a change waits for its next
  restart): set it in place, restart the machine when the change needs that
  (on Proxmox, cores or memory without hotplug), confirm what it runs with,
  then record it. The disk only grows; a shrink is refused at plan time. A
  stopped machine is not changed: start it first.
- **any other change:** not done. It is reported and the exit status is 1.
- **unknown:** a machine an earlier apply left half-built is finished (or,
  if the manifest no longer declares it, removed). A machine the manifest no
  longer declares (or that destroy is removing) and that the provider does
  not list any more keeps its record unless `--forget-gone` (see below). Any
  other unknown line is never touched, is reported as not done, and the exit
  status is 1. That includes a declared machine removed by hand: v0.1
  neither rebuilds it nor forgets it.

**Exit status:**
- **0:** done, or nothing to do.
- **1:** the plan has errors, you declined, or a machine failed or was not done.
- **2:** accrete could not plan or start. That covers bad flags, a bad manifest
  or state, no token, a token the provider refused (401), and a state locked by
  another run.

### Interruptions and retries

apply writes the state before the first request for each machine and after
every step: clone, seed, start, confirm. Each write is atomic, so a crash or
a lost connection leaves a record of where that machine stood.

- **Plan:** a machine that was half-built shows as `unknown`.
- **Apply again:** it finishes that machine with the same minted UUID and owner
  mark, so the clone is found and never made twice.
- **destroy:** it removes a half-built machine once that machine carries its
  identity. If nothing was built, it drops the record. A clone that was never
  stamped is left in place, with a message saying to finish it with apply.
- **Unconfirmed machines:** destroy never touches a machine the provider does
  not confirm, for example a guest at its VMID that no longer carries its
  identity. Such a machine stays recorded and destroy exits 1.
- **Machines already gone:** a machine to delete (no longer declared, or
  being destroyed) that the provider no longer lists (removed by hand, or by
  a delete whose answer was lost) also stays recorded, and the run exits 1.
  So does a half-built machine whose guest is no longer listed. Proxmox
  leaves out guests the token cannot see, so an empty VMID is no proof the
  machine is gone. Once you have checked that it is, run the same command
  with `--forget-gone` to retire its record.
- **Lock:** a `<state>.lock` file stops two runs on one state. A lock left by a
  crashed run on the same host is taken over. Any other lock is reported and
  never removed for you.

When a step's answer is lost (a timeout, a dropped connection, a gateway
error), accrete says that the outcome is unknown. Run the same command again
and it carries on from the state.

## Manifest reference

```yaml
apiVersion: accretion/v1alpha1
kind: Environment
metadata:
  name: <environment name>            # records are kept per environment
spec:
  provider:
    type: proxmox                     # the only provider in v0.1
    endpoint: https://host:8006       # https only
    verify_tls: true                  # false for a self-signed certificate
    token_env: VAR                    # or token_file: path (relative to the manifest)
  variables:                          # optional; used as {{var.name}} in inputs
    node: pve
  group: <name>                       # optional; accepted machines are recorded as members
  resources:
    - name: web                       # unique within the environment
      count: 2                        # default 1
      blueprint: provision-proxmox-vm
      power: "on"                     # "on" (default) or "off"
      inputs: { … }                   # see below
```

In string inputs, `{{instance.index}}` (0, 1, …) and `{{instance.number}}`
(1, 2, …) tell instances apart. A resource with `count` above 1 whose
instances would be identical is refused.

**Inputs of `provision-proxmox-vm`:**

| Input | Default | Notes |
| --- | --- | --- |
| `pve_node` | `pve` | The node to clone on. The template must be on it. |
| `template_vmid` | (required) | A cloud-init template. |
| `hostname` | (required) | DNS-label characters. Also the guest's name. |
| `cores` | 2 | |
| `memory_mb` | 2048 | |
| `disk_gb` | 0 | Grows the boot disk to at least this. 0 keeps the template's. |
| `cpu` | `x86-64-v2-AES` | A CPU model name. |
| `net0` | `virtio,bridge=vmbr0` | The first network card, for example `virtio,bridge=vmbr0,tag=30`. |
| `net1`..`net3` | none | More cards, numbered without a gap. Each names its bridge. |
| `ip_address` | none (DHCP) | The first card's fixed address with its prefix, `10.10.0.10/24`. `ip_address1`..`ip_address3` do the same for the other cards; a card without one takes DHCP. |
| `gateway` | none | The default route, through the first card; needs `ip_address`, and must be in its network. |
| `nameservers` | none | Comma-separated, with a fixed address. |
| `pool` | none | A resource pool the clone is created in. Grant the token's machine privileges on `/pool/<name>` to confine it there; `accrete permissions` prints the grants. Set at creation only. |
| `snippet_storage` | `local` | A storage that takes ISO content. The cloud-init seed lives here. |
| `bootstrap` | none | `users` (name, `ssh_keys`, `sudo`, `shell`), `packages`, `timezone`, `files`, `run`. Written as cloud-config. |

### Install from an installer ISO: `install-proxmox-vm`

A machine can also be installed from a vendor installer, unattended, with no
keyboard: accrete creates an empty VM, attaches the installer on `ide2` and a
small disc of the machine's own labelled `OEMDRV` on `ide3`, holding a
kickstart rendered from the manifest. Anaconda reads that disc by itself, so
nothing is ever typed at a boot menu. RHEL 9 (and its rebuilds) is the first
supported installer.

```yaml
    - name: ks
      blueprint: install-proxmox-vm
      inputs:
        pve_node: pve
        hostname: db-{{instance.number}}
        iso: local:iso/rhel-9.6-x86_64-dvd.iso
        memory_mb: 3072
        bootstrap:
          users:
            - name: ops
              ssh_keys: ["ssh-ed25519 AAAA… ops@laptop"]
              sudo: true
```

| Input | Default | Notes |
| --- | --- | --- |
| `pve_node` | `pve` | |
| `hostname` | (required) | The VM's name and the hostname the kickstart sets. |
| `iso` | (required) | The installer's volume. accrete never removes it. |
| `iso_sha256` | none | Recorded as declared. accrete cannot hash a file on the node's storage, so it never says it checked it. |
| `answer_format` | `kickstart` | The only format this release writes. |
| `install_timeout_minutes` | 60 | How long one apply waits for the installer to power the machine off. |
| `seed_storage` | `local` | Takes ISO content; holds the answer disc until the install is done. |
| `disk_storage` | `local-lvm` | Where the empty disk is made. |
| `disk_gb` | 32 | The only disk the kickstart partitions. |
| `cores`, `memory_mb`, `cpu`, `net0` | 2, 2048, `x86-64-v2-AES`, `virtio,bridge=vmbr0` | memory_mb is at least 1536. DHCP on the one card. |
| `bootstrap` | none | `users`, `packages`, `timezone`, `files`. `run` is refused (its commands would run in the installer, not on the booted machine), and so is any `{{secret.…}}` (the disc is readable from storage). |

The kickstart is fixed on purpose: text mode, DHCP, root locked, only the new
disk partitioned, the minimal environment plus `qemu-guest-agent`, and
`poweroff` at the end, never `reboot`.

**The steps, each saved before the next:** create, answer, start-installer,
installing, remove-media, boot, accept. A killed accrete is resumed by the
next apply on the same machine; the installer carries on meanwhile.

**Accepted only on evidence.** A power-off alone is not "installed". The
machine is accepted once the installer powered it off, both discs were
removed, it started from its own disk, and its guest agent reports the
declared hostname on the VM with the minted SMBIOS UUID. Otherwise apply exits
1, the record stays `unknown` and the machine is kept: run apply again to
check again, or destroy to remove it. An install still running at the deadline
is kept as it is, with its discs.

**What is recorded:** the installer's volume, size and date as Proxmox lists
them, the answer file's SHA-256, any declared ISO hash marked as declared, and
the hostname the agent reported (`installs` in the state).

**Permissions:** `accrete permissions` lists what an install needs. Unlike a
clone, `VM.GuestAgent.Audit` on `/vms` is required (on Proxmox VE 8,
`VM.Monitor`): the hostname it reads is what the install is accepted on.

Sybil does not run this blueprint yet: a manifest that uses it is refused there
by name ("not one this connector offers").

### Build from a disk image, any OS: `image-proxmox-vm`

Most operating systems publish a cloud image: a disk that boots straight
into the installed system and reads its first-boot config from a small disc
labelled `cidata`. accrete builds a machine from such an image with no
template made by hand and no installer: the VM's disk is a copy of the image,
grown to `disk_gb`, and its seed disc holds the config rendered from the
manifest, in one of two formats:

- `seed_format: ignition` for **Fedora CoreOS** (its `proxmoxve` image reads
  an Ignition config from the `cidata` disc) and other Ignition systems;
- `seed_format: cloud-init` (the default) for Debian, Ubuntu, Rocky, Alma,
  Fedora Cloud and other cloud-init images.

```yaml
    - name: fcos
      blueprint: image-proxmox-vm
      inputs:
        pve_node: pve
        hostname: fcos-{{instance.number}}
        image: local:import/fedora-coreos-44.20261023.3.1-proxmoxve.x86_64.qcow2
        seed_format: ignition
        disk_gb: 20
        memory_mb: 4096
        bootstrap:
          users:
            - name: core
              ssh_keys: ["ssh-ed25519 AAAA… ops@laptop"]
              sudo: true
```

**accrete fetches the image itself.** Give `image_url` and `image_sha256`:
when the storage does not have `image` yet, apply downloads the file on the
machine running accrete, checks its SHA-256 against the manifest before
anything is sent, decompresses it (`.xz` or `.gz`) and uploads it as `image`.
A file whose hash differs is never uploaded, and an image already there is
never overwritten. For Fedora CoreOS, `accrete image fedora-coreos [--stream
stable] [--storage local]` prints those inputs for the current release, read
from Fedora's stream metadata. The one thing left to do in Proxmox, once, is to
let the storage hold disk images (Proxmox VE 8.2 or later): Datacenter →
Storage → `local` → Edit → Content → add **Import**. apply names it when it is
missing. See `examples/fedora-coreos.yaml`.

| Input | Default | Notes |
| --- | --- | --- |
| `pve_node` | `pve` | |
| `hostname` | (required) | The VM's name and the hostname its seed sets. |
| `image` | (required) | `storage:import/name.qcow2` (or `.raw`, `.vmdk`). accrete never removes it. |
| `image_url` | none | Where to fetch the image from when the storage lacks it (`http` or `https`; `.xz` and `.gz` are decompressed). Needs `image_sha256`. |
| `image_sha256` | none | With `image_url`: the SHA-256 of the file at that URL, checked on download. Without: recorded as declared, since accrete cannot hash a file on the node's storage. |
| `seed_format` | `cloud-init` | `cloud-init` or `ignition`. |
| `first_boot_timeout_minutes` | 15 | How long one apply waits for the first boot to power the machine off. |
| `seed_storage` | `local` | Takes ISO content; holds the machine's seed disc. |
| `disk_storage` | `local-lvm` | Where the copy of the image is made. |
| `disk_gb` | 0 | The boot disk is grown to this; 0 keeps the image's size. Grows only. |
| `cores`, `memory_mb`, `cpu`, `net0` | 2, 2048, `x86-64-v2-AES`, `virtio,bridge=vmbr0` | DHCP on the one card. A serial port is added: these images log their console there too. |
| `bootstrap` | none | `users`, `timezone`, `files` in both formats; `packages` and `run` with cloud-init only. With Ignition, packages are refused (an image-based OS installs none at first boot: run the software as a container, or layer it later with rpm-ostree), so are `run` (write a systemd unit with `files`) and files under `/usr` (read-only). Any `{{secret.…}}` is refused: the disc is readable from storage. |

**The steps, each saved before the next:** create, guest-init,
start-first-boot, first-boot, boot, confirm. A killed accrete is resumed by
the next apply on the same machine.

**Accepted on the guest's own sign, not on "it runs".** The seed ends the
first boot with a power-off once its config has been applied: cloud-init's
`power_state` at the end of its final stage, or, for Ignition, a one-shot
unit (`accrete-first-boot.service`) that runs once the boot reached
`multi-user.target`. A machine whose Ignition config failed stops in its
emergency shell and never powers off, so it is not accepted: apply exits 1,
the record stays `unknown` and the machine is kept. Once powered off, the
machine is started again and accepted if it runs with its minted SMBIOS UUID
and the declared cores and memory. Fedora CoreOS ships no guest agent, so
nothing more is read from inside it; when an image has one, its hostname must
be the declared one.

**What is recorded:** the image's volume, size and date as Proxmox lists them,
the seed's format and SHA-256, and the image hash: checked, with its URL, when
this build fetched the image; otherwise marked as declared (`images` in the
state).

**Permissions:** no template and no guest agent. `Datastore.Audit` on the
image's storage (to find and copy it), the disk storage, the seed storage,
`/vms` (including `VM.Config.HWType` for the serial port), and
`Datastore.AllocateTemplate` on the image's storage when accrete is to upload
it. `accrete permissions` lists them.

Sybil does not run this blueprint yet.

`plan` also understands `networks:`, `nics:`, `image:`, `placement:` and
`firewall:`, as Sybil does. `apply` creates missing `networks:` (see above)
and writes none of the others (see below).

### Declare what already runs: `accrete init --from-cluster`

```sh
accrete init --from-cluster --provider proxmox --endpoint https://pve.example.lan:8006 \
  --token-env ACCRETE_PROXMOX_TOKEN --verify-tls=false --name lab [--select '^web-'] [--template 9000]
```

It reads the cluster (GET requests only) and writes `env.yaml` and
`accrete.state.json` for the VMs it finds, then runs `accrete plan` on them.
Machines named `web-1` to `web-n` with the same size and settings become one
resource with `count: n`; every other VM is a resource of its own. Each VM is
recorded as adopted and tied to the SMBIOS UUID it carries now, so from then
on it is planned, drift-checked and destroyed like a machine accrete built.

- Nothing is guessed. A full clone does not record its template, so unless
  `--template` is given (or the VM is a linked clone), `template_vmid` is left
  as a variable (`template_<resource>`) and the plan says it is missing.
- A VM with no SMBIOS UUID, or whose name is not a DNS label, is left out
  with the reason printed. A stopped VM is declared `power: "off"`.
- init never overwrites a file.

### Who changed it

A line that says a machine was changed outside accrete (a size that drifted,
or a machine powered off when the manifest says on) also says who made that
change, read from Proxmox's task log:

```
  change   web/0  (pve1/qemu/100)  changed outside Sybil: memory_mb is 2048, the manifest declares 1024
             memory_mb: 2048 -> 1024
             changed by hamza@pve at 2026-11-01T13:00:00Z (qmconfig)
```

It never guesses. When the newest recorded change of that kind is accrete's
own, when none is recorded, or when the newest power change does not explain
the state (a shutdown typed inside the guest leaves no task), the line says
`changed by: unknown` and why. Proxmox records no task for a config change
made through its synchronous API, and lists others' tasks only to a token
with `Sys.Audit` on the node. JSON output carries it as `changed_by`.

## Next to Terraform: `accrete verify --terraform-state`

A fleet Terraform owns stays Terraform's. verify only reads, and it answers a
question `terraform plan` does not: is what the state records still what runs,
and if not, who changed it?

```sh
terraform -chdir=terraform show -json > state.json     # or pass terraform.tfstate, or its directory
accrete verify --terraform-state state.json --provider proxmox \
  --endpoint https://pve.example.lan:8006 --verify-tls=false --token-env ACCRETE_PROXMOX_TOKEN
```

```
Terraform state state.json (show-json, written by Terraform 1.14.8) against https://pve.example.lan:8006
  matches  module.sdn.proxmox_sdn_vnet.vnets["mgmt"]  (vmgmt)
  differs  module.vms["mgmt-01"].proxmox_virtual_environment_vm.this  (pve1/qemu/202)
             net1 is on the machine, not in the state (bridge=vweb, link=up, mac=bc:24:11:00:02:12, vlan=30)
             cores: the state records 2, the provider reports 4
             changed by hamza@pve at 2026-11-08T09:30:00Z (qmconfig)
  unknown  module.vms["gone-01"].proxmox_virtual_environment_vm.this  the provider lists no vm qemu/203; it may be gone, or the token may not see it
  matches  module.vms["web-01"].proxmox_virtual_environment_vm.this  (pve1/qemu/201)
not in the Terraform state:
  vm       db-01  (pve1/qemu/101)
  not verified (accrete does not map these types): proxmox_sdn_zone_simple (8)
  2 match, 1 differ, 1 unknown, 1 not in the state, 8 not verified
```

- **Input.** The output of `terraform show -json` (or `tofu show -json`) is
  the stable format, and the one to use with a remote backend; `-` reads it
  from standard input. A local `terraform.tfstate` (format 4), or the
  directory holding it, is read too, as a best effort.
- **What is compared.** For bpg/proxmox `proxmox_virtual_environment_vm`: the
  VMID, name, node, SMBIOS UUID (when the state records one), cores, memory,
  each disk's size, and each network card's bridge, MAC, VLAN tag and link,
  with cards and disks the machine has and the state does not. For
  `proxmox_sdn_vnet`: that the vnet exists, in its zone. Other types are
  counted as not verified. Machines (not templates) the provider runs that
  no resource records are listed; with `--select`, they are not.
- **Power** is compared only with `--terraform-power`: bpg/proxmox records
  `started` as the configuration asks, which is often in
  `lifecycle.ignore_changes`.
- **Who changed it** uses the same rule as `plan` (see below), so a change
  Proxmox applied without a task, such as a card added through the API, is
  `changed by: unknown`. A config task is evidence of a config change, not
  of which field it touched.
- **Secrets.** Values Terraform marks sensitive are dropped while the state is
  read. Nothing from the state is written anywhere.
- **One owner.** `plan`, `apply` and `destroy` take `--terraform-state` too.
  A line that would act on a machine the state records, or create a machine
  with the name of one, is then an error, so apply and destroy refuse the plan
  before sending anything. `init --from-cluster --terraform-state` leaves the
  machines Terraform owns out of the manifest it writes.
- **Never written.** verify sends GET requests only and writes neither the
  Terraform state nor an accrete state. A resource stays Terraform's: accrete
  never gives a machine a second owner.

### Handing machines over, both ways

Each direction goes through Terraform's own blocks. accrete never edits a
Terraform state and never runs `terraform` for you.

**From Terraform to accrete:** `accrete import --from-terraform STATE` (the
same as `accrete init --from-terraform`) declares only the VMs that state
records (`--select` matches their name or Terraform address), adopts each
under the SMBIOS UUID it carries now, and writes `terraform-handover.tf`
beside the manifest:

```hcl
removed {
  from = proxmox_virtual_environment_vm.batch

  lifecycle {
    destroy = false
  }
}
```

Remove those machines from the Terraform configuration and put the file
beside it. `terraform plan` must then say each "will no longer be managed by
Terraform" and destroy nothing. A `removed` block names every instance under
an address, so when only some instances of a `for_each` are handed over
(`module.vms["web-1"]`), accrete prints `terraform state rm '<address>'` for
each instead, for you to run. Until Terraform has forgotten them, both tools
record the machines: apply neither.

**From accrete to Terraform:** `accrete export --terraform -f env.yaml` writes
`terraform-import.tf` with one `import {}` block per machine (bpg/proxmox,
`id = "<node>/<vmid>"`). Run `terraform plan -generate-config-out=generated.tf`
and `terraform apply`: Terraform writes the resources and imports them,
creating nothing. Then remove the resources from the accrete manifest and run
`accrete export --retire --terraform-state <the new state> -f env.yaml`. It
retires accrete's record of each machine that state now records, and keeps
(and names) any that it does not record or that the manifest still declares.
## Network installs and shared answer files

`accrete pxe` and `accrete bundle` install machines from an installer, VMs or
physical machines alike, without anyone choosing an entry in a BIOS, UEFI or
boot menu. They read a `NetworkInstall` file (see
[examples/network-install](examples/network-install)) and contact nothing.

- **One answer file for many machines.** Each machine's `ks.cfg` profile is
  selected when it boots, from its SMBIOS UUID, serial number or a MAC. A
  machine that matches no profile, or two, stops in `%pre` before any disk is
  partitioned. Profiles hold no secret: `{{secret.*}}` is refused, because
  every machine that boots the medium can read every profile.
- **`accrete bundle -f install.yaml`** writes that `ks.cfg` for every
  declared machine. Put it on a disc or USB stick labelled `OEMDRV` beside
  the installer.
- **`accrete pxe -f install.yaml`** writes `gate.ipxe`, `ks.cfg` and
  `dnsmasq-proxy.conf`. The proxy DHCP assigns no address, so your DHCP
  server is not changed. The gate boots the installer only for machines with
  `armed: true`, and sends every other machine back to its own disk. Serve
  `gate.ipxe`, `ks.cfg` and the installer's `images/pxeboot` files under
  `spec.server`, and run `dnsmasq -k -C dnsmasq-proxy.conf` on a machine on
  `spec.subnet`. Once a machine is installed, set `armed: false` and run
  `accrete pxe` again, so its next network boot goes to its disk.

Proven on Proxmox on 2026-11-06 with RHEL 9.6: one VM that was not armed
booted its own disk, and one armed VM installed over the network with no
keyboard and then booted from its disk. Not yet tried on physical hardware.
UEFI Secure Boot must be off for `ipxe.efi`. Prefer UUID or MAC selectors on
Proxmox, which sets no SMBIOS serial by default.

`provider/redfish` (BMC virtual media, one-time boot, power, guarded by the
declared serial or UUID) is built and tested against the sushy-tools
emulator only. No command uses it yet.

## What v0.1 does not do

apply refuses these before sending anything, and exits 1:

- **Changing a network.** A network that plans as `change` or `unknown`
  (another subnet, gateway, NAT or DHCP range, another zone, not up on a
  node) is refused: accrete creates missing networks, and never changes or
  replaces one, which would re-plumb every machine on it.
- **Firewalls.** A resource that declares `firewall:` is refused.
- **Secrets.** A bootstrap that references `{{secret.name}}` is refused.
  accrete has no secret store.
- **Other blueprints.** Only `provision-proxmox-vm`, `install-proxmox-vm`
  and `image-proxmox-vm` are built. `provision-proxmox-iso-vm` (an installer with no answer file) and
  the template-building blueprint are refused by name.

Also not done in v0.1:

- **Changing a built machine's other inputs** (template, network, hostname,
  bootstrap). Such a line plans as `change` and is reported as not done.
  Destroy and re-apply.
- **Cross-node clones.** A machine is cloned on its template's node.
- **Agents.** "Accepted" in accrete means what the provider observes: the
  machine runs, carries its minted SMBIOS UUID, and has the declared cores and
  memory. No agent enrols and nothing inside the guest is checked. Importing
  the manifest and state into Sybil re-evaluates acceptance with an agent.
- **Adopting one machine into an existing manifest.** `accrete init
  --from-cluster` and `accrete import --from-terraform` declare and adopt
  what runs (see above), into a new manifest.
- **Providers other than Proxmox.**

## For provider authors

A provider implements `provider.Provider` and `provider.Session` to be planned
against. To be applied with, it also implements `provider.Writer` (Clone,
GuestInit, Power, Delete) and declares `Capabilities.Write`. Two suites define
the contract:
- `providertest.Contract` covers reads;
- `providertest.WriteContract` covers create, idempotent retry, an
  interrupted clone resumed without a duplicate, a lost clone answer, refusals
  under the wrong identity, and delete twice.

`provider/fake` passes both, and so does `provider/proxmox` against its
in-memory PVE (`provider/proxmox/pvetest`).

## Tests

```sh
go test ./...
```

Nothing here calls a real hypervisor.
