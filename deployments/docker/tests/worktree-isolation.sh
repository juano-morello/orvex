#!/usr/bin/env bash
# Run from any directory in the checkout containing the launcher under test.
set -euo pipefail
declare -A fingerprint
source_root=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
root=$tmp/repo
cleanup() {
  for label in a b; do
    dir=$tmp/$label
    if [[ -x $dir/workspace ]]; then "$dir/workspace" dispose; fi
    if [[ -f $dir/.git ]]; then git -C "$root" worktree remove --force "$dir"; fi
  done
  if [[ -x $root/workspace ]]; then "$root/workspace" gc; fi
  rm -r -- "$tmp"
}
trap cleanup EXIT
git clone --quiet --no-hardlinks "$source_root" "$root"
# Run collection in a private repository, including on failed test runs.
cp "$source_root/workspace" "$root/"
mkdir -p "$root/.devcontainer"
cp "$source_root/.devcontainer/compose.yaml" "$root/.devcontainer/"
cp "$source_root/deployments/docker/"{compose.yaml,demo-credentials.py,unrevealed-rounds.sql} "$root/deployments/docker/"
for label in a b; do
  dir=$tmp/$label
  git -C "$root" worktree add --quiet --detach "$dir" HEAD
  # Exercise uncommitted launcher edits with an otherwise clean migration history.
  cp "$root/workspace" "$dir/"
  mkdir -p "$dir/.devcontainer"
  cp "$root/.devcontainer/compose.yaml" "$dir/.devcontainer/"
  cp "$root/deployments/docker/"{compose.yaml,demo-credentials.py,unrevealed-rounds.sql} "$dir/deployments/docker/"
  migrations=("$dir"/migrations/*.up.sql)
  last=${migrations[-1]##*/}
  printf -v version '%04d' "$((10#${last%%_*} + 1))"
  base="$dir/migrations/${version}_probe_$label"
  echo "CREATE TABLE orvex.probe_$label (id integer);" > "$base.up.sql"
  echo "DROP TABLE orvex.probe_$label;" > "$base.down.sql"
done
"$tmp/a/workspace" preview > "$tmp/a.log" & a=$!
"$tmp/b/workspace" preview > "$tmp/b.log" & b=$!
failed=0
wait "$a" || failed=1
wait "$b" || failed=1
for label in a b; do
  grep -F "${version} probe_$label applied" "$tmp/$label.log" || failed=1
done
((failed == 0))
# Each preview owns a credentials volume for the host user, and no two share a key.
creds=/var/lib/orvex-preview
want="$(id -u):$(id -g) 700"
for label in a b; do
  got=$("$tmp/$label/workspace" exec stat -c '%u:%g %a' "$creds")
  [[ $got == "$want" ]] || { echo "$label credentials directory is $got, want $want" >&2; exit 1; }
  [[ $("$tmp/$label/workspace" exec stat -c %a "$creds/keyring") == 600 ]]
  fingerprint[$label]=$("$tmp/$label/workspace" exec sha256sum "$creds/keyring")
done
[[ ${fingerprint[a]} != "${fingerprint[b]}" ]] || { echo 'previews share a keyring' >&2; exit 1; }
# An old dev container has no credentials mount. Recovery must write to its
# project's volume without replacing the container that may still hold a key.
(
  cd "$tmp/b"
  umask 077
  ./workspace exec cat "$creds/keyring" > "$tmp/original-keyring"
  ./workspace exec rm "$creds/keyring"
  sql=deployments/docker/unrevealed-rounds.sql
  cp "$sql" "$tmp/original-query"
  echo 'SELECT 1 / 0;' > "$sql"
  if ./workspace preview > "$tmp/query-failure.log" 2>&1; then
    echo 'preview created credentials after the database check failed' >&2
    exit 1
  fi
  ./workspace exec test ! -e "$creds/keyring"
  mv "$tmp/original-query" "$sql"
  ./workspace exec rm "$creds/gateway-token"
  export ORVEX_WORKSPACE_ID ORVEX_UID ORVEX_GID
  ORVEX_WORKSPACE_ID=$(basename "$(git rev-parse --absolute-git-dir)"/orvex-wt-*)
  ORVEX_UID=$(id -u)
  ORVEX_GID=$(id -g)
  cat > "$tmp/legacy.yaml" <<'EOF'
services:
  dev:
    volumes: !override ["../..:/workspace"]
    depends_on: !reset {}
EOF
  compose=(docker compose -p "$ORVEX_WORKSPACE_ID" -f deployments/docker/compose.yaml -f .devcontainer/compose.yaml)
  "${compose[@]}" -f "$tmp/legacy.yaml" up -d --no-deps --force-recreate dev
  old_dev=$("${compose[@]}" ps -q dev)
  docker exec "$old_dev" python3 -c 'import secrets
from pathlib import Path
p = Path("/tmp/.local/state/orvex-demo/gateway-token")
p.parent.mkdir(mode=0o700, parents=True)
p.write_text(secrets.token_hex(32) + "\n")
p.chmod(0o600)'
  docker exec "$old_dev" cat /tmp/.local/state/orvex-demo/gateway-token | sha256sum > "$tmp/token-before"
  ./workspace restore-keyring "$tmp/original-keyring"
  [[ $("${compose[@]}" ps -q dev) == "$old_dev" ]]
  if ./workspace restore-keyring "$tmp/original-keyring"; then
    echo 'restore overwrote the stored keyring' >&2
    exit 1
  fi
)
# Recreating the development container and repeating the preview keep the key.
"$tmp/b/workspace" recreate
"$tmp/b/workspace" exec cat "$creds/gateway-token" | sha256sum > "$tmp/token-after"
cmp "$tmp/token-before" "$tmp/token-after"
"$tmp/b/workspace" preview > "$tmp/b2.log"
[[ $("$tmp/b/workspace" exec sha256sum "$creds/keyring") == "${fingerprint[b]}" ]] || { echo 'recreate changed the keyring' >&2; exit 1; }
project=$(basename "$(git -C "$tmp/a" rev-parse --absolute-git-dir)"/orvex-wt-*)
git -C "$root" worktree remove --force "$tmp/a"
"$root/workspace" gc
"$root/workspace" gc
for kind in container network volume; do
  flags=(-q)
  if [[ $kind == container ]]; then flags+=(-a); fi
  remaining=$(docker "$kind" ls "${flags[@]}" --filter "label=com.docker.compose.project=$project")
  [[ -z $remaining ]] || { echo "$project still has $kind resources" >&2; exit 1; }
done
"$tmp/b/workspace" exec ./bin/crashctl migrate status | grep -F "${version} probe_b applied"
[[ $("$tmp/b/workspace" exec sha256sum "$creds/keyring") == "${fingerprint[b]}" ]] || { echo 'collection changed the other preview keyring' >&2; exit 1; }
