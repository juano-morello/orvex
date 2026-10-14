# ADR-007: Repository Architecture & Build Strategy

Source: https://linear.app/lbtworks/document/adr-007-repository-architecture-and-build-strategy-8612450e0996

Status: Accepted
Date: 2026-10-14

## Context

Orvex consists of a deterministic Go game core, separately deployable Go gateway and engine services, a replay/audit CLI, shared gRPC protocol definitions, a React/Vite console, PostgreSQL migrations, integration/load/chaos tests, and architectural documentation. Audit replay requires reproducible historical rules and traceable builds.

## Decision

Use a **Go-first monorepo** in a single GitHub repository, with **one Go module** initially. Go `internal/` packages define code boundaries; independent binaries deploy separately. Use React + Vite + TypeScript with pnpm for the frontend. Use a Makefile for orchestration, Buf for protobuf, Docker Compose for local dependencies, GitHub Actions for CI, multi-stage Dockerfiles, and initially Kustomize for Kubernetes. Do not introduce Turborepo, Nx, multiple Go modules, or a Go workspace without demonstrated need.

## Proposed layout

```text
orvex/
  cmd/{gateway,engine,crashctl}/
  internal/game/{contract.go,registry.go,crash/}/
  internal/runtime/{admission,execution,ownership,sessions}/
  internal/persistence/{postgres,eventstore}/
  internal/audit/{replay,verification}/
  internal/transport/{grpc,http,websocket}/
  proto/orvex/v1/
  web/
  migrations/
  tests/{integration,load,chaos}/
  deployments/{docker,kubernetes}/
  docs/{adr,architecture,experiments}/
  AGENTS.md
  go.mod
  Makefile
  README.md
```

The layout is a starting convention, not an obligation to create empty directories.

## Reproducibility and historical rules

After the first stable rules release, retain immutable versioned Crash rules implementations (for example `internal/game/crash/v1/`), golden vectors, pinned dependency versions including `cockroachdb/apd/v3`, arithmetic configuration, canonical serialization definitions, and reproducible build identifiers. The audit verifier selects the historical rules version rather than interpreting all events with the latest implementation. One repository commit provides a useful platform-wide source reference, but immutable release artifacts and dependency provenance remain necessary for strong reproducibility.

## CI and commands

Provide `make test`, `make test-integration`, `make lint`, `make proto`, `make dev`, `make build`, and `make audit` as they become meaningful. Use path-aware CI for fast checks: Go domain/integration, frontend typecheck/UI, protobuf contract generation, migration integration. Historical rules or replay changes require full compatibility checks; releases run the complete correctness suite including distributed failure tests.

## Trade-offs

Pros: atomic cross-service/protocol changes, shared tests and ADRs, simpler onboarding, single commit provenance, no inter-repo release coordination. Costs: larger repository, path-aware CI complexity, potential coupling, and need to protect historical rules from accidental modification. Monorepo does not imply monolithic deployment.

## Consequences / follow-up

Mirror ADR-001–007 in `docs/adr/` after the repository exists. Put agent guidance in `AGENTS.md`, requiring review of relevant ADRs before changes and explicit flagging of conflicts. [LBT-5](https://linear.app/lbtworks/issue/LBT-5/scaffold-go-module-and-repository-conventions) is the initial implementation issue. Extract separate modules/repositories only if real independent release or reuse requirements arise.
