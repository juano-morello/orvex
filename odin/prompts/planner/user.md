# Plan Task

## Variables

### prompt

{{prompt}}

### previous_envelope

{{previous_envelope}}

### context_handoff_dir

{{context_handoff_dir}}

### workspace_map

{{workspace_map}}

## Task

Plan the work described in `prompt`. The plan must name the tests to add or update for every behavior change it calls for — the builder treats an unnamed test as optional.

1. Write the full plan to `<context_handoff_dir>/plan.md` — this is the copy the builder reads.
2. Copy that file into the repo under `docs/specs/` (this repo keeps specs under `docs/`, never a root `specs/` directory):
   - **List `docs/specs/` before you pick the name.** A session that plans more than once reuses its `<run_id>`, so the obvious name may already be taken.
   - Base name: `docs/specs/<run_id>_<slug>.md`, where `<run_id>` is the session directory name inside `context_handoff_dir` (`.../sessions/<run_id>/context_handoff`) and `<slug>` is two to four kebab-case words naming the work.
   - If a file with that name already exists, use `docs/specs/<run_id>_<slug>_v2.md`, then `_v3`, and so on until the name is free. **Never overwrite an existing spec** — the earlier plan is the record of what was asked for then.
   - **Copy it, do not retype it.** One bash call does the whole step:
     `mkdir -p docs/specs && cp "<context_handoff_dir>/plan.md" "docs/specs/<run_id>_<slug>.md"`
     Writing the plan a second time through `write` re-emits every line you already wrote, which costs the whole document again in output tokens and lets the two copies drift.
3. Rate the change's size as `S`, `M`, or `L` — this picks which roster the builder (and every
   agent after you) runs on for the rest of this run, so rate the WORK the plan describes, never
   the prompt's length or how it was phrased:
   - **S** — a localized change in 1-2 files, no new concepts: a copy edit, a config toggle, a one-line bugfix.
   - **M** — several files or a new module, moderate reasoning: a new endpoint, a small feature with its own tests.
   - **L** — cross-cutting, a design decision, or subtle correctness: a schema migration, concurrency, security, a refactor touching many call sites.
4. Emit your `Report` JSON, declaring BOTH paths in `artifacts` and your `size` rating.

## Report

Respond with ONLY valid JSON matching `PlanOutput` — no prose before or after:

```json
{
  "status": "success",
  "summary": "<one sentence describing the plan>",
  "size": "<S, M, or L — see Task step 3>",
  "artifacts": ["<context_handoff_dir>/plan.md", "docs/specs/<run_id>_<slug>.md"],
  "commit_message": "<conventional commit for THIS PLAN DOCUMENT, not the work it describes — 'docs(spec): <subject>', imperative, subject <=72 chars — e.g. 'docs(spec): add spec for the /health endpoint'>",
  "notes_for_next_agent": "<what the builder must know>"
}
```

Both `artifacts` entries are the paths you ACTUALLY wrote, `_v2` suffix and all. Gates open these files — a name you meant to use fails them.
