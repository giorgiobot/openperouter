#!/usr/bin/env bash
#
# Mirrors into this fork the branches behind the open pull requests of the
# upstream repository: the head of every open pull request, plus the base
# branch it targets, so that a mirror pull request opened here shows the same
# diff as the upstream one.
#
# Branches are pushed as the upstream commit objects themselves, so a mirror
# branch and its upstream counterpart always share the exact same head SHA.
#
# Every branch is mirrored on its own, so one failure does not stop the others.
# The failures are reported at the end and fail the job.
#
# Reads UPSTREAM_REPO and MIRROR_PREFIX from the environment, and expects an
# origin remote it can push to and a gh authenticated against GitHub.

set -uo pipefail

upstream_url="https://github.com/${UPSTREAM_REPO}"
work_ref="refs/mirror/work"

declare -A source_of
declare -A current
failed=()
updated=0
unchanged=0
deleted=0

main() {
  local pulls
  if ! pulls="$(list_open_pulls)"; then
    echo "::error::cannot list the open pull requests of ${UPSTREAM_REPO}"
    return 1
  fi
  # Pruning every mirror on an empty answer would close every mirror pull
  # request, and an upstream with no open pull request at all is not a case
  # worth handling silently.
  if [ -z "$pulls" ]; then
    echo "::error::${UPSTREAM_REPO} reports no open pull request, refusing to prune every mirror"
    return 1
  fi

  local number head_ref base_ref
  while IFS=$'\t' read -r number head_ref base_ref; do
    # The head is taken from refs/pull/<n>/head rather than from the branch,
    # because a pull request opened from another fork has no branch here.
    source_of["${MIRROR_PREFIX}/${head_ref}"]="refs/pull/${number}/head"
    source_of["${MIRROR_PREFIX}/${base_ref}"]="refs/heads/${base_ref}"
  done <<<"$pulls"

  local sha ref
  while read -r sha ref; do
    current["${ref#refs/heads/}"]="$sha"
  done < <(git ls-remote --heads origin "refs/heads/${MIRROR_PREFIX}/*")

  local branch
  for branch in "${!source_of[@]}"; do
    mirror_branch "$branch" "${source_of[$branch]}" "${current[$branch]:-}" ||
      failed+=("$branch")
  done
  for branch in "${!current[@]}"; do
    [ -z "${source_of[$branch]+set}" ] || continue
    drop_branch "$branch" || failed+=("$branch (delete)")
  done

  report "$(wc -l <<<"$pulls")"
}

list_open_pulls() {
  gh api --paginate "repos/${UPSTREAM_REPO}/pulls?state=open&per_page=100" \
    --jq '.[] | "\(.number)\t\(.head.ref)\t\(.base.ref)"'
}

mirror_branch() {
  local branch="$1" upstream_ref="$2" current_sha="$3"

  git update-ref -d "$work_ref" 2>/dev/null
  if ! git fetch --no-tags --force "$upstream_url" "${upstream_ref}:${work_ref}"; then
    return 1
  fi

  local sha
  sha="$(git rev-parse "$work_ref")"
  if [ "$sha" = "$current_sha" ]; then
    echo "${branch} is already at ${sha}"
    unchanged=$((unchanged + 1))
    return 0
  fi

  if ! git push --force origin "${work_ref}:refs/heads/${branch}"; then
    return 1
  fi
  updated=$((updated + 1))
  return 0
}

drop_branch() {
  local branch="$1"

  echo "deleting ${branch}, no open upstream pull request is behind it"
  if ! git push origin --delete "refs/heads/${branch}"; then
    return 1
  fi
  deleted=$((deleted + 1))
  return 0
}

report() {
  local open_pulls="$1"

  {
    echo "### Mirror of ${UPSTREAM_REPO}"
    echo
    echo "- open pull requests: ${open_pulls}"
    echo "- branches updated: ${updated}"
    echo "- branches already up to date: ${unchanged}"
    echo "- branches deleted: ${deleted}"
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
