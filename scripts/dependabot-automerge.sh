#!/usr/bin/env bash
# Decides whether a completed CI run for a Dependabot pull request may be
# auto-merged, and performs the merge when it is safe.
#
# This script is intentionally run from the trusted default branch by a
# `workflow_run` job. It never checks out or executes pull-request code; it only
# reads pull-request metadata and file contents through the GitHub API and then
# merges the exact commit that CI validated.
#
# The caller must pass EXPECTED_HEAD_SHA = the immutable
# github.event.workflow_run.head_sha of the completed CI run. If the pull request
# head has moved since that run, the script leaves the pull request open instead
# of merging an untested commit.
#
# Eligibility is deliberately narrow:
#   - author is dependabot[bot] and the head branch is a dependabot branch
#   - only go.mod and/or go.sum changed
#   - the module path is unchanged
#   - the direct dependency set is unchanged; Dependabot may add or remove
#     `// indirect` entries (tidy churn), but never a new direct dependency
#   - CLIProxyAPI stays on the v8 module path
#   - no replace/exclude directives are introduced
#
# The caller requires a successful full CI run (unit tests plus the minimum and
# latest real-host integration jobs) before this script runs, so ABI/schema
# compatibility is already verified for the exact head commit.
set -euo pipefail

cpa_module="github.com/router-for-me/CLIProxyAPI/v8"
plugin_module="github.com/fengwk/cliproxyapi-opencode-provider"

go_mod_module() {
	awk '$1 == "module" { print $2; exit }' <<<"$1"
}

go_mod_deps() {
	awk '
		/^require[ \t]*\(/ { block = 1; next }
		block && /^[ \t]*\)/ { block = 0; next }
		block {
			if ($1 == "" || $1 ~ /^\/\//) next
			print $1
			next
		}
		/^require[ \t]+[^ \t(]/ {
			if ($2 != "") print $2
		}
	' <<<"$1"
}

# Direct dependencies only: entries carrying the standard "// indirect" marker
# are excluded, so Dependabot's tidy churn on indirect requires is tolerated.
go_mod_direct_deps() {
	awk '
		function indirect() { return $0 ~ /[ \t]\/\/[ \t]*indirect([ \t]|$)/ }
		/^require[ \t]*\(/ { block = 1; next }
		block && /^[ \t]*\)/ { block = 0; next }
		block {
			if ($1 == "" || $1 ~ /^\/\// || indirect()) next
			print $1
			next
		}
		/^require[ \t]+[^ \t(]/ {
			if ($2 != "" && !indirect()) print $2
		}
	' <<<"$1"
}

# guard_decision <author> <branch> <changed-files> <base-go.mod> <head-go.mod>
# Prints "approved" or a rejection reason and returns 0/1 respectively.
guard_decision() {
	local author="$1" branch="$2" files="$3" base="$4" head="$5"

	if [[ "$author" != 'dependabot[bot]' ]]; then
		echo "not auto-mergeable: author is '$author', not dependabot[bot]"
		return 1
	fi
	if [[ "$branch" != dependabot/* ]]; then
		echo "not auto-mergeable: '$branch' is not a dependabot branch"
		return 1
	fi

	local file any=0
	while IFS= read -r file; do
		[[ -n "$file" ]] || continue
		any=1
		case "$file" in
		go.mod | go.sum) ;;
		*)
			echo "not auto-mergeable: unexpected changed file '$file'"
			return 1
			;;
		esac
	done <<<"$files"
	if [[ "$any" -eq 0 ]]; then
		echo "not auto-mergeable: no changed files reported"
		return 1
	fi

	local base_module head_module
	base_module="$(go_mod_module "$base")"
	head_module="$(go_mod_module "$head")"
	if [[ -z "$head_module" || "$base_module" != "$head_module" ]]; then
		echo "not auto-mergeable: module path changed ('$base_module' -> '$head_module')"
		return 1
	fi
	if [[ "$head_module" != "$plugin_module" ]]; then
		echo "not auto-mergeable: unexpected module path '$head_module'"
		return 1
	fi

	local base_direct head_direct head_deps
	base_direct="$(go_mod_direct_deps "$base" | sort -u)"
	head_direct="$(go_mod_direct_deps "$head" | sort -u)"
	if [[ "$base_direct" != "$head_direct" ]]; then
		echo "not auto-mergeable: direct dependency set changed; only version bumps are automatic"
		return 1
	fi

	head_deps="$(go_mod_deps "$head" | sort -u)"
	if ! grep -qxF "$cpa_module" <<<"$head_deps"; then
		echo "not auto-mergeable: CLIProxyAPI dependency is not on the v8 module path"
		return 1
	fi

	local base_rx head_rx
	base_rx="$(grep -E '^[[:space:]]*(replace|exclude)([[:space:]]|$)' <<<"$base" || true)"
	head_rx="$(grep -E '^[[:space:]]*(replace|exclude)([[:space:]]|$)' <<<"$head" || true)"
	if [[ "$base_rx" != "$head_rx" ]]; then
		echo "not auto-mergeable: replace/exclude directives changed"
		return 1
	fi

	echo "approved"
	return 0
}

main() {
	local repo="${GH_REPO:-}" pr="${PR_NUMBER:-}"
	if [[ -z "$repo" || -z "$pr" ]]; then
		echo "dependabot-automerge: GH_REPO and PR_NUMBER are required" >&2
		return 2
	fi

	# The tested commit must be supplied by the workflow; never fall back to the
	# current pull request head, which may have advanced past the successful run.
	local expected_head="${EXPECTED_HEAD_SHA:-}"
	if [[ -z "$expected_head" ]]; then
		echo "dependabot-automerge: EXPECTED_HEAD_SHA (workflow_run.head_sha) is required" >&2
		return 2
	fi

	local gh="${GH_BIN:-gh}"

	local author branch base_sha current_head
	author="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.user.login')"
	branch="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.head.ref')"
	base_sha="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.base.sha')"
	current_head="$("$gh" api "repos/${repo}/pulls/${pr}" --jq '.head.sha')"

	local files base head
	files="$("$gh" api "repos/${repo}/pulls/${pr}/files" --paginate --jq '.[].filename')"
	base="$("$gh" api "repos/${repo}/contents/go.mod?ref=${base_sha}" -H 'Accept: application/vnd.github.raw')"
	head="$("$gh" api "repos/${repo}/contents/go.mod?ref=${current_head}" -H 'Accept: application/vnd.github.raw')"

	local decision
	if ! decision="$(guard_decision "$author" "$branch" "$files" "$base" "$head")"; then
		echo "dependabot-automerge: ${decision}; leaving the pull request open for a human"
		return 0
	fi

	if [[ "$current_head" != "$expected_head" ]]; then
		echo "dependabot-automerge: pull request head moved from the CI-tested commit ${expected_head} to ${current_head}; leaving the pull request open"
		return 0
	fi
	echo "dependabot-automerge: ${decision}; enabling squash auto-merge for tested commit ${expected_head}"

	local state
	state="$("$gh" pr view "$pr" --repo "$repo" --json state,autoMergeRequest \
		--jq '.state + " " + (if .autoMergeRequest == null then "none" else "enabled" end)')"
	case "$state" in
	MERGED\ *) echo "dependabot-automerge: pull request is already merged"; return 0 ;;
	OPEN\ enabled) echo "dependabot-automerge: auto-merge is already enabled"; return 0 ;;
	esac

	"$gh" pr merge "$pr" --repo "$repo" --squash --auto --match-head-commit "$expected_head"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
