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
cp "$source_root/deployments/docker/compose.yaml" "$source_root/deployments/docker/demo-credentials.py" "$root/deployments/docker/"
for label in a b; do
  dir=$tmp/$label
  git -C "$root" worktree add --quiet --detach "$dir" HEAD
  # Exercise uncommitted launcher edits with an otherwise clean migration history.
  cp "$root/workspace" "$dir/"
  mkdir -p "$dir/.devcontainer"
  cp "$root/.devcontainer/compose.yaml" "$dir/.devcontainer/"
  cp "$root/deployments/docker/compose.yaml" "$root/deployments/docker/demo-credentials.py" "$dir/deployments/docker/"
  last=$(basename "$(ls "$dir"/migrations/*.up.sql | tail -1)")
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
  "$tmp/$label/workspace" exec sh -c 'test "$(stat -c %a '"$creds"'/keyring)" = 600 && test -r '"$creds"'/keyring'
  fingerprint[$label]=$("$tmp/$label/workspace" exec sha256sum "$creds/keyring")
done
[[ ${fingerprint[a]} != "${fingerprint[b]}" ]] || { echo 'previews share a keyring' >&2; exit 1; }
# Recreating the development container and repeating the preview keep the key.
"$tmp/b/workspace" recreate
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
