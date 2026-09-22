#!/usr/bin/env bash
#
# Mirrors every branch of the upstream repository into this fork, under the
# MIRROR_PREFIX prefix, so that pull requests can be opened here against the
# exact same commits.
#
# Branches are pushed as the upstream commit objects themselves, so a mirror
# branch and its upstream counterpart always share the exact same head SHA.
#
# Mirrors are never removed: a branch stays here once it is mirrored, so that
# deleting it upstream does not take the work away from under the pull requests
# opened against it.
#
# Every branch is mirrored on its own, so one failure does not stop the others.
# The failures are reported at the end and fail the job.
#
# Reads UPSTREAM_REPO and MIRROR_PREFIX from the environment, and expects an
# origin remote it can push to.

set -uo pipefail

upstream_url="https://github.com/${UPSTREAM_REPO}"
upstream_refs="refs/mirror/upstream"

declare -A current
failed=()
updated=0
unchanged=0

main() {
  if ! git fetch --no-tags --prune --force "$upstream_url" "refs/heads/*:${upstream_refs}/*"; then
    echo "::error::cannot fetch the branches of ${UPSTREAM_REPO}"
    return 1
  fi

  local sha ref
  while read -r sha ref; do
    current["${ref#refs/heads/}"]="$sha"
  done < <(git ls-remote --heads origin "refs/heads/${MIRROR_PREFIX}/*")

  local branch
  while read -r sha ref; do
    branch="${MIRROR_PREFIX}/${ref#"${upstream_refs}/"}"
    mirror_branch "$branch" "$sha" "${current[$branch]:-}" || failed+=("$branch")
  done < <(git for-each-ref --format='%(objectname) %(refname)' "$upstream_refs")

  report
}

mirror_branch() {
  local branch="$1" sha="$2" current_sha="$3"

  if [ "$sha" = "$current_sha" ]; then
    unchanged=$((unchanged + 1))
    return 0
  fi

  echo "mirroring ${branch} at ${sha}"
  if ! git push --force origin "${sha}:refs/heads/${branch}"; then
    return 1
  fi
  updated=$((updated + 1))
  return 0
}

report() {
  {
    echo "### Mirror of ${UPSTREAM_REPO}"
    echo
    echo "- branches updated: ${updated}"
    echo "- branches already up to date: ${unchanged}"
    echo "- branches failed: ${#failed[@]}"
  } >>"${GITHUB_STEP_SUMMARY:-/dev/stdout}"

  [ ${#failed[@]} -gt 0 ] || return 0

  local branch
  for branch in "${failed[@]}"; do
    echo "::error::failed to mirror ${branch}"
  done
  return 1
}

main "$@"
