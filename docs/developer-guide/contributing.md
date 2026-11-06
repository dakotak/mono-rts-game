# Contributing

## Setup

You need Go 1.26.2 or newer; nothing else. See
[installation](../getting-started/installation.md).

```bash
make lint test      # what every change must pass
make race           # before touching concurrency (server, hub, engine queue)
```

The repository has no CI configuration, so these commands are the gate.

## Repository conventions

- **Formatting:** `gofmt` (`make fmt`). `make lint` checks `cmd`,
  `internal`, `web` and `examples`, and also runs `go vet`, `staticcheck`
  and `buf lint`.
- **Protobuf:** edit `proto/`, run `make generate`, and commit the
  regenerated `gen/`. Fields are only ever **added**. Check compatibility
  against `main` with:

  ```bash
  go tool -modfile=tools/go.mod buf breaking --against '.git#branch=main'
  ```

- **Tools** are pinned in `tools/go.mod` and run with
  `go tool -modfile=tools/go.mod <tool>`. Don't add global tool
  requirements.
- **Kernel purity:** `internal/sim` imports only the standard library. No
  game rules, no protobuf, no networking.
- **New mechanics are plugins.** Follow the
  [plugin tutorial](plugins/getting-started.md). A plugin's commands need a
  proto package under `proto/rts/<plugin>/v1` and an entry in
  [`internal/protoreg`](../../internal/protoreg/protoreg.go); a test fails
  if you forget.
- **Determinism is non-negotiable.** Read
  [determinism.md](determinism.md) and
  [plugins/state-and-determinism.md](plugins/state-and-determinism.md).
  Every new mechanic needs a replay test.
- **Information hiding.** Anything a player must not know stays
  owner-only, and errors about other players' entities use
  `sim.Unavailable`. With fog on, foreign data flows only through the
  visibility plugin's disclosure rules.
- **Bounded work.** No per-tick loop may grow with something a player
  controls without a cap.
- **Docs:** update the relevant page in `docs/` with the code, including
  the [configuration](../reference/configuration.md),
  [commands](../reference/commands.md) and [events](../reference/events.md)
  references.

## Checklist for a change

- [ ] Tests for new behavior, including failure paths and a determinism or
      replay test for anything that runs in a tick
- [ ] `make lint test` passes; `make race` if concurrency was touched
- [ ] Proto changes regenerated, backward compatible, and listed in
      `protoreg`
- [ ] Config fields validated in `Register`, with defaults and ranges
      documented
- [ ] No hidden-information leak: attribute visibility, event audiences,
      error messages
- [ ] Docs updated

## Commits

Commit messages in the history follow a summary line plus a body
explaining what and why (see `git log`).

## License

Contributions are licensed under the repository's
[GNU AGPL v3](../../LICENSE).
