#!/usr/bin/env bash
# Unit tests for scripts/dependabot-automerge.sh.
#
# Part 1 exercises the pure pull-request guard (the security boundary) with
# fabricated inputs. Part 2 drives main() against a fake `gh` binary to prove the
# wiring: it only merges after the guard approves, it refuses stale CI runs whose
# pull request head moved, it fails closed when the tested head is missing, it
# merges immediately for the exact tested commit (never `--auto`, which the
# repository has disabled), it skips an already-merged pull request, and it
# dispatches the main-branch CI only after a successful merge.
# No network or GitHub access is required.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091 # sourced from the same directory at runtime
source "${script_dir}/dependabot-automerge.sh"

base_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.16
	gopkg.in/yaml.v3 v3.0.1
)
'

cpa_bump_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.17
	gopkg.in/yaml.v3 v3.0.1
)
'

new_dep_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.17
	github.com/example/new-dependency v1.0.0
	gopkg.in/yaml.v3 v3.0.1
)
'

indirect_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.17
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/example/transitive v1.2.3 // indirect
'

dropped_direct_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.17
)
'

v9_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v9 v9.0.0
	gopkg.in/yaml.v3 v3.0.1
)
'

replace_go_mod='module github.com/fengwk/cliproxyapi-opencode-provider

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.17
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/router-for-me/CLIProxyAPI/v8 => ../local-fork
'

failures=0
case_number=0

run_guard_case() {
	local expected_rc="$1" description="$2" author="$3" branch="$4" files="$5" base="$6" head="$7"
	case_number=$((case_number + 1))
	local rc=0
	guard_decision "$author" "$branch" "$files" "$base" "$head" >/dev/null 2>&1 || rc=$?
	if [[ "$rc" -eq "$expected_rc" ]]; then
		printf 'ok   %d - %s\n' "$case_number" "$description"
	else
		printf 'FAIL %d - %s (rc=%d want=%d)\n' "$case_number" "$description" "$rc" "$expected_rc"
		failures=$((failures + 1))
	fi
}

run_guard_case 0 "CPA v8 version bump is auto-mergeable" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod"

run_guard_case 0 "go.sum-only dependency update is auto-mergeable" \
	'dependabot[bot]' 'dependabot/go_modules/gopkg.in/yaml.v3-def' \
	'go.sum' "$base_go_mod" "$base_go_mod"

run_guard_case 1 "non-dependabot author is rejected" \
	'octocat' 'dependabot/go_modules/x-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod"

run_guard_case 1 "non-dependabot branch is rejected" \
	'dependabot[bot]' 'feature/manual-bump' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod"

run_guard_case 1 "workflow file change is rejected" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum\n.github/workflows/ci.yml' "$base_go_mod" "$cpa_bump_go_mod"

run_guard_case 1 "empty change list is rejected" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	'' "$base_go_mod" "$cpa_bump_go_mod"

run_guard_case 1 "new dependency is rejected" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/example/new-dependency-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$new_dep_go_mod"

run_guard_case 0 "indirect dependency churn is auto-mergeable" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$indirect_go_mod"

run_guard_case 1 "dropped direct dependency is rejected" \
	'dependabot[bot]' 'dependabot/go_modules/gopkg.in/yaml.v3-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$dropped_direct_go_mod"

run_guard_case 1 "CPA v9 module switch is rejected" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v9-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$v9_go_mod"

run_guard_case 1 "replace directive is rejected" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$replace_go_mod"

# --- main() wiring against a fake gh -----------------------------------------

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cat >"${work}/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
case "$args" in
*"pulls/7 --jq .user.login"*) printf '%s\n' "$FAKE_AUTHOR" ;;
*"pulls/7 --jq .head.ref"*) printf '%s\n' "$FAKE_BRANCH" ;;
*"pulls/7 --jq .base.sha"*) printf '%s\n' 'basesha' ;;
*"pulls/7 --jq .head.sha"*) printf '%s\n' "$FAKE_CURRENT_HEAD" ;;
*"pulls/7/files"*) printf '%s\n' "$FAKE_FILES" ;;
*"contents/go.mod?ref=basesha"*) printf '%s' "$FAKE_BASE_GOMOD" ;;
*"contents/go.mod?ref="*) printf '%s' "$FAKE_HEAD_GOMOD" ;;
*"pr view"*) printf '%s\n' "$FAKE_PR_STATE" ;;
*"pr merge"*)
	printf '%s\n' "$args" >>"$FAKE_GH_LOG"
	exit "${FAKE_MERGE_RC:-0}"
	;;
*"workflow run"*) printf '%s\n' "$args" >>"$FAKE_DISPATCH_LOG" ;;
*)
	echo "fake gh: unexpected args: $args" >&2
	exit 3
	;;
esac
FAKE_GH
chmod +x "${work}/gh"

# run_main_case <expect-merge> <expect-dispatch> <description> <author> <branch> \
#               <files> <base> <head> <expected-head> <current-head> [expect-rc]
# CASE_PR_STATE / CASE_MERGE_RC optionally override the fake pull request state
# and the merge command's exit status for a single case, then reset to defaults.
run_main_case() {
	local expect_merge="$1" expect_dispatch="$2" description="$3" author="$4" branch="$5" files="$6"
	local base="$7" head="$8" expected_head="$9" current_head="${10}" expect_rc="${11:-0}"
	local fake_state="${CASE_PR_STATE:-OPEN}" fake_merge_rc="${CASE_MERGE_RC:-0}"
	CASE_PR_STATE="OPEN"
	CASE_MERGE_RC="0"
	case_number=$((case_number + 1))
	local log="${work}/merge-$case_number.log"
	local dispatch="${work}/dispatch-$case_number.log"
	: >"$log"
	: >"$dispatch"
	local rc=0
	(
		export GH_BIN="${work}/gh" GH_REPO="owner/repo" PR_NUMBER="7"
		export EXPECTED_HEAD_SHA="$expected_head"
		export FAKE_AUTHOR="$author" FAKE_BRANCH="$branch" FAKE_FILES="$files"
		export FAKE_BASE_GOMOD="$base" FAKE_HEAD_GOMOD="$head"
		export FAKE_CURRENT_HEAD="$current_head"
		export FAKE_PR_STATE="$fake_state" FAKE_MERGE_RC="$fake_merge_rc"
		export FAKE_GH_LOG="$log" FAKE_DISPATCH_LOG="$dispatch"
		main
	) >/dev/null 2>&1 || rc=$?

	local merged="no"
	[[ -s "$log" ]] && merged="yes"
	local matched="yes"
	if [[ "$merged" == "yes" ]]; then
		grep -qF -- "--match-head-commit ${expected_head}" "$log" || matched="no"
		grep -qF -- "--squash" "$log" || matched="no"
		# Auto-merge is disabled on the repository, so it must never be requested.
		if grep -qF -- "--auto" "$log"; then matched="no"; fi
	fi

	local dispatched="no"
	[[ -s "$dispatch" ]] && dispatched="yes"
	local dispatch_ok="yes"
	if [[ "$dispatched" == "yes" ]]; then
		grep -qF -- "ci.yml" "$dispatch" || dispatch_ok="no"
		grep -qF -- "--ref main" "$dispatch" || dispatch_ok="no"
	fi

	if [[ "$rc" -eq "$expect_rc" && "$merged" == "$expect_merge" && \
		"$dispatched" == "$expect_dispatch" && "$matched" == "yes" && "$dispatch_ok" == "yes" ]]; then
		printf 'ok   %d - %s\n' "$case_number" "$description"
	else
		printf 'FAIL %d - %s (rc=%d want=%d merge=%s want=%s dispatch=%s want=%s match=%s dispatch_ok=%s)\n' \
			"$case_number" "$description" "$rc" "$expect_rc" "$merged" "$expect_merge" \
			"$dispatched" "$expect_dispatch" "$matched" "$dispatch_ok"
		failures=$((failures + 1))
	fi
}

run_main_case yes yes "eligible dependabot bump merges the tested commit and dispatches main CI" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

run_main_case no no "human pull request never reaches the merge" \
	'octocat' 'feature/manual-bump' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

run_main_case no no "dependabot change outside go.mod/go.sum is not merged" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum\nREADME.md' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

# Stale CI run: the pull request advanced after CI succeeded, so the new head is
# untested and must never be merged.
run_main_case no no "stale CI run with a newer PR head is not merged" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'testedsha' 'newersha'

# Fail closed when the workflow did not provide the tested head commit.
run_main_case no no "missing expected head fails closed" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" '' 'headsha' 2

# Already merged: idempotent no-op, and no redundant CI dispatch.
CASE_PR_STATE="MERGED"
run_main_case no no "already merged pull request is skipped without dispatching CI" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

# Failed merge (e.g. blocked by branch protection): never dispatch CI.
CASE_MERGE_RC="1"
run_main_case yes no "a failed merge does not dispatch main CI" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha' 1

if [[ "$failures" -ne 0 ]]; then
	printf '\n%d test(s) failed\n' "$failures" >&2
	exit 1
fi
printf '\nall %d automerge tests passed\n' "$case_number"
