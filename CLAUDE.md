# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

```bash
# Full clean-clone gate. It installs/builds the embedded admin assets first.
make check

# Concurrency-sensitive backend validation.
make test-race

# Production and complete dependency audits.
cd web && npm audit --omit=dev && npm audit
```

`make check` is the canonical gate. Do not run `go test ./...` as the first
command in a clean checkout because `internal/admin` embeds `dist`, which is
created by `web-build`.

## Architecture Overview

Free Router is a modular monolith. Keep it a single process unless deployment
requirements materially change. The composition root is `cmd/root.go`.

```text
cmd (composition)
├── gateway -> adapter / eligibility / routing / catalog / health / provider / transport
└── admin   -> eligibility / routing / catalog / health / provider / credentials
                  catalog -> provider
```

Important boundaries:

- `gateway.CandidatePlanner` owns candidate ordering and reads one immutable
  `eligibility.Snapshot` per planning pass.
- `gateway.AttemptExecutor` owns one upstream attempt, including adapter
  normalization, fallback classification, response streaming, health, and
  metrics.
- `catalog.ProbeRunner` builds and executes capability probes. `catalog.Store`
  remains the compatibility facade for inventory and evidence.
- Catalog mutations follow persist-before-publish. Build the next state, atomically
  replace the cache file, then expose the new in-memory snapshot.
- `admin.ConfigService` and `admin.CredentialService` coordinate management
  transactions. HTTP handlers only decode input and map service errors.
- Provider wire protocols are selected through `adapter.Resolver`. A provider
  may declare `adapter` in its spec. The default remains OpenAI-compatible.

### State authority

| State | Authority | Projection/consumer |
| --- | --- | --- |
| Maintained model inventory | embedded/custom free-model manifest | `catalog.Snapshot` |
| Quarantine and capability evidence | atomic catalog cache | `catalog.Store` |
| Route aliases and model overrides | routing config with revision CAS | `eligibility.Snapshot` |
| Runtime circuit/latency state | `health.Tracker` | eligibility, Gateway, Admin |
| Effective routability and reason | `eligibility.Snapshot` | `/v1/models`, candidate planner, `/admin/api/state` |
| Provider credentials | OS/file vault | provider registry reload |

Do not duplicate effective-routability rules in handlers or UI code. Add new
conditions to `eligibility.Snapshot` and cover Gateway/Admin consistency.

## Conventions & Patterns

- Preserve OpenAI endpoint behavior, routing JSON, cache schema, and Admin JSON
  fields when refactoring. New response fields should be additive.
- Prefer immutable snapshots and narrow interfaces over framework-style DI.
- Keep provider-specific wire behavior in adapters. Do not create provider class
  hierarchies for OpenAI-compatible services.
- Every persistent mutation must have a failure-path test proving runtime state
  remains unchanged when disk publication fails.
- Frontend polling must not overwrite an unsaved route draft. Keep that policy in
  `web/src/useConfigDraft.ts`.
