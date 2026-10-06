#!/usr/bin/env bash
# Unit tests for scripts/dependabot-automerge.sh.
#
# Part 1 exercises the pure pull-request guard (the security boundary) with
# fabricated inputs. Part 2 drives main() against a fake `gh` binary to prove the
# wiring: it only merges after the guard approves, it refuses stale CI runs whose
# pull request head moved, and it fails closed when the tested head is missing.
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
*"pr view"*) printf '%s\n' 'OPEN none' ;;
*"pr merge"*) printf '%s\n' "$args" >>"$FAKE_GH_LOG" ;;
*)
	echo "fake gh: unexpected args: $args" >&2
	exit 3
	;;
esac
FAKE_GH
chmod +x "${work}/gh"

# run_main_case <expect-merge> <description> <author> <branch> <files> <base> <head> \
#               <expected-head> <current-head> [expect-rc]
run_main_case() {
	local expect_merge="$1" description="$2" author="$3" branch="$4" files="$5"
	local base="$6" head="$7" expected_head="$8" current_head="$9" expect_rc="${10:-0}"
	case_number=$((case_number + 1))
	local log="${work}/merge-$case_number.log"
	: >"$log"
	local rc=0
	(
		export GH_BIN="${work}/gh" GH_REPO="owner/repo" PR_NUMBER="7"
		export EXPECTED_HEAD_SHA="$expected_head"
		export FAKE_AUTHOR="$author" FAKE_BRANCH="$branch" FAKE_FILES="$files"
		export FAKE_BASE_GOMOD="$base" FAKE_HEAD_GOMOD="$head"
		export FAKE_CURRENT_HEAD="$current_head" FAKE_GH_LOG="$log"
		main
	) >/dev/null 2>&1 || rc=$?

	local merged="no"
	[[ -s "$log" ]] && merged="yes"
	local matched="yes"
	if [[ "$merged" == "yes" ]]; then
		grep -qF -- "--match-head-commit ${expected_head}" "$log" || matched="no"
	fi

	if [[ "$rc" -eq "$expect_rc" && "$merged" == "$expect_merge" && "$matched" == "yes" ]]; then
		printf 'ok   %d - %s\n' "$case_number" "$description"
	else
		printf 'FAIL %d - %s (rc=%d want=%d merged=%s want=%s match=%s)\n' \
			"$case_number" "$description" "$rc" "$expect_rc" "$merged" "$expect_merge" "$matched"
		failures=$((failures + 1))
	fi
}

run_main_case yes "eligible dependabot bump invokes auto-merge for the tested commit" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

run_main_case no "human pull request never reaches auto-merge" \
	'octocat' 'feature/manual-bump' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

run_main_case no "dependabot change outside go.mod/go.sum is not merged" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum\nREADME.md' "$base_go_mod" "$cpa_bump_go_mod" 'headsha' 'headsha'

# Stale CI run: the pull request advanced after CI succeeded, so the new head is
# untested and must never be merged.
run_main_case no "stale CI run with a newer PR head is not merged" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" 'testedsha' 'newersha'

# Fail closed when the workflow did not provide the tested head commit.
run_main_case no "missing expected head fails closed" \
	'dependabot[bot]' 'dependabot/go_modules/github.com/router-for-me/CLIProxyAPI/v8-abc' \
	$'go.mod\ngo.sum' "$base_go_mod" "$cpa_bump_go_mod" '' 'headsha' 2

if [[ "$failures" -ne 0 ]]; then
	printf '\n%d test(s) failed\n' "$failures" >&2
	exit 1
fi
printf '\nall %d automerge tests passed\n' "$case_number"
