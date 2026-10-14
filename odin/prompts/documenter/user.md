# Document Task

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

Document the completed work described by `previous_envelope`, using `prompt` for what was originally asked.

1. Read the full diff at `previous_envelope.diff_path`, plus any changed file that needs context.
2. Write the write-up to `<context_handoff_dir>/document.md`. Cover: what changed and why it matters, the files that carry it, and how to use or verify it.
3. Copy that file into the repo under `docs/changes/` (this repo keeps write-ups under `docs/`; any instruction mentioning `app_docs/` means `docs/changes/` here — never create a root `app_docs/` directory):
   - **List `docs/changes/` before you pick the name.** A session that documents more than once reuses its `<run_id>`, so the obvious name may already be taken.
   - Base name: `docs/changes/<run_id>_<slug>.md`, where `<run_id>` is the session directory name inside `context_handoff_dir` (`.../sessions/<run_id>/context_handoff`) and `<slug>` is two to four kebab-case words naming the work.
   - If a file with that name already exists, use `docs/changes/<run_id>_<slug>_v2.md`, then `_v3`, and so on until the name is free. **Never overwrite an existing write-up** — it describes a change that already shipped.
   - **Copy it, do not retype it.** One bash call does the whole step:
     `mkdir -p docs/changes && cp "<context_handoff_dir>/document.md" "docs/changes/<run_id>_<slug>.md"`
     Writing the document a second time through `write` re-emits every line you already wrote, which costs the whole write-up again in output tokens and lets the two copies drift.
4. Emit your `Report` JSON, declaring BOTH paths in `artifacts`.

## Report

Respond with ONLY valid JSON matching `DocumentOutput` — no prose before or after:

```json
{
  "status": "success",
  "summary": "<one sentence describing what you documented>",
  "document_path": "docs/changes/<run_id>_<slug>.md",
  "documented_files": ["src/server.ts"],
  "artifacts": ["<context_handoff_dir>/document.md", "docs/changes/<run_id>_<slug>.md"],
  "commit_message": "<conventional commit for THIS WRITE-UP, not the change it describes — 'docs: <subject>', imperative, subject <=72 chars — e.g. 'docs: document the /health endpoint'>",
  "notes_for_next_agent": "<anything the diff left unexplained>"
}
```

`document_path` and the `docs/changes/` entry in `artifacts` are the path you ACTUALLY wrote, `_v2` suffix and all. Gates open these files — a name you meant to use fails them.
