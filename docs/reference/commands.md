# Command reference

Every command type accepted by the default server. Wrap the payload in
`google.protobuf.Any` (`type.googleapis.com/<type>`) and send it with
`GameService.SubmitCommand`, or `AdminService.SubmitAdminCommand` for
admins. Proto sources are in [`proto/rts/<plugin>/v1`](../../proto/rts).

| Column | Meaning |
|---|---|
| **Who** | `player`: anyone, acting on their own units. `admin`: admin-only, rejected for players at submission. |
| **Batch** | Has `ApplyToUnit`, so it works as a `BatchCommand` template. |
| **Cost** | Unit-commands charged against the per-tick budget. Admins are never charged. |

Every command is **atomic** unless the table says otherwise: if it fails,
nothing changed. Unit lists hold 1..4096 IDs with no duplicates. Units
you may not command produce `not_found`.

## Movement (`rts.movement.v1`)

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `MoveCommand` | `orders[] {unit_id, destination}` | player | — | 1 per order | Persistent order per unit; replaces the previous one. Destinations must be in bounds. |
| `StopCommand` | `unit_ids[]` | player | ✓ | 1 per unit | Cancels movement orders. |
| `GroupMoveCommand` | `unit_ids[]`, `destination` | player | — | 1 per unit that gets a slot | Distinct reachable tiles around the destination. **Partial**: per-unit results; fails only if no unit could be moved. |

## Energy (`rts.energy.v1`)

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `MineCommand` | `unit_ids[]`, `well_id` | player | ✓ | 1 per unit | Persistent; extracts only while adjacent. The well must be detected. |
| `StopMiningCommand` | `unit_ids[]` | player | ✓ | 1 per unit | |
| `TransferCommand` | `from_unit_id`, `to_unit_id`, `amount` | player | — | 1 | Both units yours and adjacent at execution; `amount > 0`; replaces the sender's transfer order. |
| `SetUnitEnergyCommand` | `unit_id`, `energy`, optional `capacity` | admin | — | — | Sets a store directly. |

## Wells (`rts.wells.v1`)

| Command | Fields | Who | Notes |
|---|---|---|---|
| `CreateWellCommand` | `position`, optional `energy`, `capacity`, `extraction_rate`, `unlimited` | admin | Unset fields use `wells.defaults`. |
| `ConfigureWellCommand` | `well_id`, optional fields as above | admin | Only set fields change; `0 ≤ energy ≤ capacity`. |

## Attributes, upgrades, cloning

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `rts.attributes.v1.SetAttributeCommand` | `unit_id`, `attribute`, `value` | admin | — | — | Within bounds; not counted as invested. |
| `rts.upgrades.v1.UpgradeCommand` | `unit_ids[]`, `attribute`, `target` \| `increase` | player | ✓ | 1 per unit | Paid in full up front (`insufficient_energy` otherwise); irreversible; one project per unit. |
| `rts.cloning.v1.CloneCommand` | `unit_ids[]`, `direction` (`DIRECTION_*`, unspecified = automatic) | player | ✓ | 1 per unit | Pays `cost + offspring_energy` up front; needs a free adjacent tile at start. |

## Metadata and batches (`rts.metadata.v1`)

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `SetMetadataCommand` | `unit_ids[]`, `key`, `value` | player | ✓ | 1 per unit | Up to `max_entries` tags per unit; key `^[a-z0-9][a-z0-9_.-]*$`. |
| `RemoveMetadataCommand` | `unit_ids[]`, `key` | player | ✓ | 1 per unit | |
| `BatchCommand` | `selector {unit_ids[], conditions[] {key, match, value}, limit, owner_id}`, `command` (Any template) | player | — | charged per unit by the template | Selector evaluated at execution, over your own units only (`owner_id` is admin-only). `match`: `MATCH_KIND_EQUALS`, `PRESENT`, `ABSENT`. **Partial**: per-unit results. Nested batches and admin-only templates are rejected. |

## Combat and salvage

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `rts.combat.v1.AttackCommand` | `unit_ids[]`, `target_id` | player | ✓ | 1 per unit | Target must be a detected enemy unit (friendly → `permission_denied`). No pursuit. |
| `rts.combat.v1.StopAttackCommand` | `unit_ids[]` | player | ✓ | 1 per unit | |
| `rts.combat.v1.SetHealthCommand` | `unit_id`, `health` | admin | — | — | 0 destroys the unit (it leaves wreckage). |
| `rts.salvage.v1.SalvageCommand` | `unit_ids[]`, `wreck_id` | player | ✓ | 1 per unit | Collects while within distance 1; the wreck must be detected. |
| `rts.salvage.v1.StopSalvageCommand` | `unit_ids[]` | player | ✓ | 1 per unit | |
| `rts.salvage.v1.CreateWreckCommand` | `position`, `energy` | admin | — | — | |
| `rts.salvage.v1.SetWreckEnergyCommand` | `wreck_id`, `energy` | admin | — | — | 0 removes the wreck. |

## Automation (`rts.automation.v1`)

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `AutomateCommand` | `unit_ids[]`, one of: `mine_and_deliver {well_id, receiver_id, deliver_at, keep}`, `auto_salvage {radius 1..16}`, `guard {position, radius 0..16}`, `follow {leader_id, distance 1..8}` | player | ✓ | 1 per unit | Replaces the unit's behavior and clears its orders. Well must be detected; receiver and leader must be your units (not the unit itself). Guard needs attack > 0. |
| `CancelAutomationCommand` | `unit_ids[]` | player | ✓ | 1 per unit | Removes the behavior and stops the orders it drove. |

## Logistics (`rts.logistics.v1`)

| Command | Fields | Who | Batch | Cost | Notes |
|---|---|---|---|---|---|
| `SupplyCommand` | `unit_ids[]` (suppliers), `receiver_id`, `reserve`, `fill_to`, `resume_below` | player | ✓ | 1 per unit | Adds or updates a link; max 4 receivers per supplier; receiver must be your unit; cycles rejected (`failed_precondition`); `resume_below ≤ fill_to` when `fill_to > 0`. |
| `CancelSupplyCommand` | `unit_ids[]`, `receiver_id` (0 = all links) | player | ✓ | 1 per unit | |

## Kernel commands (AdminService RPCs only)

| RPC | Kernel command | Payload |
|---|---|---|
| `SpawnUnit` | `sim.CreateEntity` | Go struct (not reachable through `Any`) |
| `DeleteEntity` | `sim.RemoveEntity` | Go struct |
| `MoveEntity` | `sim.MoveEntity` | Go struct |

## Example plugin (only when registered)

| Command | Fields | Who | Batch | Cost |
|---|---|---|---|---|
| `rts.examples.regeneration.v1.SetRegenerationCommand` | `unit_ids[]`, `enabled` | player | ✓ | 1 per unit |

## Error codes

| Code | Meaning |
|---|---|
| `invalid_argument` | Malformed payload or parameter out of range |
| `not_found` | Entity missing, not yours, or not currently observable (indistinguishable on purpose) |
| `permission_denied` | Forbidden target or parameter (friendly attack target, foreign transfer receiver, selector `owner_id` from a player, admin-only batch template) |
| `failed_precondition` | Valid but impossible now (occupied tile, busy unit, cycle, no free tile, too many links, ...) |
| `resource_exhausted` | Per-tick unit-command budget exceeded |
| `insufficient_energy` | Can't pay up front (upgrade, clone) |
| `internal` | Unexpected handler error (a bug) |

Submission-time rejections (`SubmitCommandResponse.rejection_reason`)
cover unknown types, admin-only types sent by players, and undecodable
payloads. They never reach a handler.
