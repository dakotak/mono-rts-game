# Quickstart

This walkthrough runs a local two-player world, plays a few minutes of it
in the browser, and then talks to the same server from the command line.

## 1. Start the world

```bash
make dev
```

`make dev` builds `rts-server` and `rts-gateway` and starts them with
[`configs/dev.json`](../../configs/dev.json). You'll see the startup log:

```
world 32x20, 54 entities, 10 Hz, plugins [attributes metadata terrain visibility wells energy logistics movement upgrades cloning combat salvage automation]
tick schedule: automation: automation.evaluate; energy: energy.mine, energy.transfer; logistics: logistics.supply; salvage: salvage.collect; movement: movement.step; combat: combat.resolve; upgrades: upgrades.progress; cloning: cloning.progress
gRPC listening on 127.0.0.1:50051
player gateway on http://127.0.0.1:8080 -> gRPC 127.0.0.1:50051
admin gateway on http://127.0.0.1:8081 -> gRPC 127.0.0.1:50051
```

The plugin list is in registration (dependency) order. The schedule is
the per-tick execution order, which
[simulation.md](../developer-guide/simulation.md) explains.

The dev world is 32×20 tiles at 10 ticks per second. It has:

- two players, Red (`red`) and Blue (`blue`)
- a 4×3 block of units for each player, plus one extra unit each
- three energy wells, one of them unlimited
- rock walls and a marsh
- fog of war turned on

## 2. Play

1. Open http://127.0.0.1:8080, type `red` and press **Connect**. Open a
   second tab and join as `blue`.
2. You see only your own units and what they can sense. Dark tiles are
   unexplored, dimmed tiles were seen before, and outlines are "last known"
   positions of things you can no longer see.
3. **Drag** to box-select some units, then **right-click** an empty tile.
   They walk there, routing around the rock walls. Each step costs 1 energy
   (`movement.energy_per_step` in the dev config), shown by the bar under
   each unit.
4. Explore until you find a well (a resource icon). **Right-click** it with
   units selected: they walk next to it and start mining. "+N" numbers float
   up as energy arrives.
5. Select one unit and look at the sidebar. It lists attributes (armor,
   propulsion, sensors, ...), each with the exact cost of +1. Click
   **Upgrade** on one you can afford.
6. Under **Automation**, pick *Mine & deliver*, press **Assign…**, click a
   well, then click one of your units as the receiver. The selected units
   now loop on their own. **Automation status** shows what they are doing.
7. Find the other player and right-click one of their units to attack.
   Destroyed units leave wreckage, which you can right-click to salvage.

[Gameplay and the web client](../user-guide/gameplay.md) covers every
control.

## 3. Watch as an admin

Open http://127.0.0.1:8081 and sign in with `dev-admin`. The admin client
shows the whole world with no fog. You can:

- pause and single-step the simulation
- spawn units and wells
- inspect any entity
- preview what a given player sees ("View as")

See [administration](../operations/administration.md).

## 4. Talk to the API directly

The gateway exposes the player API as JSON over HTTP, so `curl` works:

```bash
# Join as red; the response carries a session token and the world rules.
TOKEN=$(curl -s -X POST localhost:8080/api/connect -d '{"joinToken":"red"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["sessionToken"])')

# Current view: your units plus what you detect.
curl -s localhost:8080/api/snapshot -H "Authorization: Bearer $TOKEN" | head -c 600; echo

# The live stream: one JSON object per line (a snapshot, then one update per tick).
curl -sN localhost:8080/api/observe -H "Authorization: Bearer $TOKEN" | head -n 3 | cut -c1-200
```

Native clients use gRPC on port 50051. The server enables gRPC reflection,
so tools like `grpcurl` can discover the services. See the
[API overview](../api/overview.md) and the
[bot guide](../api/bot-development.md), which has a complete Go bot you can
run with `go run ./examples/bot`.

## 5. Stop

Press Ctrl-C in the `make dev` terminal. All state is in memory, so the
next start is a fresh world.
