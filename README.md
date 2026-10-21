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

You need Go 1.26 or later and a clone of this repository.

```sh
cd accretion
GOWORK=off go build -o accrete ./cmd/accrete
./accrete plan -h
```

The binary is self-contained. Put it anywhere on your `PATH`. No release is
published yet.

## Quickstart on Proxmox (5 minutes)

You need:
- a Proxmox VE node;
- a cloud-init template with the QEMU guest agent installed, for example a
  Debian or Rocky cloud image turned into a template (`qm template <vmid>`);
- a storage that accepts ISO content (`local` does by default);
- a DHCP server on the bridge.

1. Create an API token. In the Proxmox UI, go to Datacenter → Permissions →
   API Tokens. The token needs these privileges on `/`, or on the node, the
   storage and a pool:
   - `VM.Allocate`, `VM.Clone`, `VM.Config.*`, `VM.PowerMgmt` and `VM.Audit`;
   - `Datastore.AllocateSpace` and `Datastore.Audit`;
   - `Sys.Audit`.

   Proxmox's built-in `PVEVMAdmin` role plus `Datastore.AllocateSpace` is
   enough.
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
| `accrete plan -f env.yaml [--state F] [-o text\|json] [--offline]` | Prints what apply would do. Sends GET requests only and writes nothing. `--offline` asks the provider nothing. |
| `accrete apply -f env.yaml [--state F] [--auto-approve]` | Prints the plan, asks, then acts on it one machine at a time. |
| `accrete destroy -f env.yaml [--state F] [--auto-approve]` | Removes every machine the state records for this environment, if the provider confirms it is still the machine that was built. |

**What apply does with each plan line:**
- **create:** clone the template, seed cloud-init, start the machine, confirm it, record it.
- **delete** (a machine the manifest no longer declares): stop it, delete it, remove its seed, retire its record.
- **change** that is a power difference: power the machine on or off.
- **any other change:** not done. It is reported and the exit status is 1.
- **unknown:** never touched.

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
| `net0` | `virtio,bridge=vmbr0` | The one network card, for example `virtio,bridge=vmbr0,tag=30`. |
| `snippet_storage` | `local` | A storage that takes ISO content. The cloud-init seed lives here. |
| `bootstrap` | none | `users` (name, `ssh_keys`, `sudo`, `shell`), `packages`, `timezone`, `files`, `run`. Written as cloud-config. |

`plan` also understands `networks:`, `nics:`, `image:`, `placement:` and
`firewall:`, as Sybil does. `apply` writes none of them in v0.1 (see below).

## What v0.1 does not do

apply refuses these before sending anything, and exits 1:

- **Networks.** A plan line for a network that is not "unchanged" is refused,
  because accrete does not create SDN zones or vnets.
- **Firewalls.** A resource that declares `firewall:` is refused.
- **Fixed addresses and more than one card.** Machines take DHCP on their first
  card. `ip_address*`, `gateway`, `nameservers` and `net1`..`net3` are refused.
- **Secrets.** A bootstrap that references `{{secret.name}}` is refused.
  accrete has no secret store.
- **Other blueprints.** Only `provision-proxmox-vm` is built. Installer-ISO and
  template-building blueprints are refused by name.

Also not done in v0.1:

- **Changing a built machine's size or inputs.** Such a line plans as `change`
  and is reported as not done. Destroy and re-apply, or wait for Resize.
- **Cross-node clones.** A machine is cloned on its template's node.
- **Agents.** "Accepted" in accrete means what the provider observes: the
  machine runs, carries its minted SMBIOS UUID, and has the declared cores and
  memory. No agent enrols and nothing inside the guest is checked. Importing
  the manifest and state into Sybil re-evaluates acceptance with an agent.
- **Adopting machines that accrete did not build.**
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
cd accretion && GOWORK=off go test ./...
```

Nothing here calls a real hypervisor.
