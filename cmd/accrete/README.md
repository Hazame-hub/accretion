# accrete

`accrete plan` plans an `accretion/v1alpha1` Environment manifest against a
local state file and what the provider it names reports now, with no Sybil,
no database and no server. It reads and never writes: it sends the provider
GET requests only, no machine is changed and the state file is not
rewritten.

## Plan the example

From the repository root, after setting `endpoint`, `node` and `template` in a copy
of `examples/two-vms.yaml` to your cluster's:

```sh
export ACCRETE_PROXMOX_TOKEN='user@realm!tokenid=secret'
go run ./cmd/accrete plan -f examples/two-vms.yaml
go run ./cmd/accrete plan -f examples/two-vms.yaml -o json
```

PowerShell:

```powershell
$s = Read-Host -AsSecureString 'API token secret'   # typed masked
$env:ACCRETE_PROXMOX_TOKEN = 'user@realm!tokenid=' + [Net.NetworkCredential]::new('', $s).Password
go run ./cmd/accrete plan -f examples/two-vms.yaml
```

With no state file, nothing is built yet, so both machines plan as create.
`--offline` plans without asking the provider at all.

## Output

The plan comes from `accretion/plan`, the planner Sybil Continuum's API uses.
`-o json` has the API's shape (`entries` with `action` create, no-op,
change, delete or unknown, `reason`, `errors`, `limits`, `summary`, `ok`)
plus accrete's own `notes`: what it could not read. `-o text` prints one line
per entry, then errors, warnings, limits, notes and the summary.

Exit status: 0 the plan has no errors, 1 it has errors, 2 accrete could not
plan (bad flags, a bad manifest or state, no token, or a token that is not
user@realm!tokenid=secret, such as the secret alone).

## The token

`spec.provider` names where the token is, never the token: `token_env`, an
environment variable, or `token_file`, a file relative to the manifest. A
token file that others than its owner can read gets a warning on stderr
(`chmod 600` it); the plan still runs. The token is never printed, in the
plan, the notes or an error.

## State

`accrete.state.json`, beside the manifest unless `--state` says otherwise,
version 1:

- `realizations`: the machines built, in `accretion/plan`'s JSON shape
  (Sybil Continuum's realization rows): `id`, `integration_id`, `asset_id`,
  `resource_ref_id` (e.g. `pve1/qemu/105`), `intent` (environment, resource,
  instance index), `intent_inputs`, `provenance` and `minted_uuid`, the
  SMBIOS UUID the machine was built with. `integration_id` is the provider's
  id, a UUIDv5 of its type and endpoint, so a state recorded against one
  cluster never matches another.
- `verdicts`: the acceptance verdict per asset (`accepted`, ...).
- `in_flight`, `groups`: optional.

It holds no endpoint and no credential. Until `accrete apply` exists, a
state is written by hand.

## What is read live, and what is not

Each recorded machine is looked for at its ref, and counts only if the
guest there still carries its minted SMBIOS UUID. Then its size (`cores`,
`memory_mb`, `disk_gb`) and, when a resource declares `power`, its power
state are compared. A recorded machine the provider does not confirm
(unreachable, token refused, nothing at the ref, another guest there) plans
as **unknown**, never as no-op: the record alone is not the machine.

Cluster capacity and SDN networks are read live. Not read yet, and stated
as limits rather than assumed: the token's network permissions, firewalls,
and `image:` lookups (use the blueprint's template input).
