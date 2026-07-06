#!/usr/bin/env bash
# release.sh drives a github release end to end via the gh cli, matching the
# behaviors described in intent/builds.md.
#
# It creates a pre-release for a ref or tag (auto-incrementing the minor version
# on HEAD of main when no tag is given), waits for the build workflow to finish,
# and then promotes the pre-release to a full release once tests pass and every
# platform's build has uploaded. Alpha verification runs are never promoted;
# instead a build for the current platform is downloaded and smoke tested so the
# workflow itself can be validated without shipping a release.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$REPO_ROOT/.ci-logs"
WORKFLOW="build.yml"

usage() {
	cat <<'EOF'
Usage: release.sh [options] [ref-or-tag]

Creates a github pre-release for ref-or-tag (default: an auto-incremented minor
tag on HEAD of main), waits for the build workflow, and promotes it to a full
release once tests pass and builds upload.

Options:
  --target <ref>  Git ref the tag is created at (default: main, or the current
                  branch for --alpha so the changes under test are built).
  --list          List previous build workflow runs for the ref or tag and exit.
  --no-promote    Leave the release as a pre-release (opt out of promotion).
  --alpha         Create an alpha pre-release to verify the workflow: never
                  promoted, and after the workflow a build for the current
                  platform is downloaded and smoke tested.
  -h, --help      Show this help and exit.
EOF
}

fail() {
	echo "release.sh: $*" >&2
	exit 1
}

# next_minor_tag prints the next vMAJOR.MINOR.0 tag after the highest existing
# semver tag, so unattended releases bump the minor version on HEAD of main.
next_minor_tag() {
	local latest major minor
	latest="$(git -C "$REPO_ROOT" tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)"
	if [ -z "$latest" ]; then
		echo "v0.1.0"
		return
	fi
	IFS=. read -r major minor _ <<<"${latest#v}"
	echo "v${major}.$((minor + 1)).0"
}

# WORKFLOW_PATH is the in-repo path the runs API reports for our workflow. The
# runs API is used instead of `gh run list --workflow` so discovery works even
# before the workflow is registered on the default branch, e.g. while verifying
# an unmerged workflow change.
WORKFLOW_PATH=".github/workflows/$WORKFLOW"

# run_id_for prints the database id of the build run for a commit sha, or
# nothing if the workflow has not started yet. Discovery keys on head_sha
# because tag-push runs report a null head_branch and cannot be found by ref
# name.
run_id_for() {
	gh api "repos/{owner}/{repo}/actions/runs?head_sha=$1&event=push" \
		--jq "[.workflow_runs[] | select(.path == \"$WORKFLOW_PATH\")][0].id // empty" 2>/dev/null
}

# list_runs shows every build run for a ref or tag, covering both tag-push and
# manual workflow_dispatch runs. A ref that resolves to a commit is matched by
# head_sha so tags (whose runs report a null head_branch) are included.
list_runs() {
	local ref="$1" query="branch=$1" sha
	if sha="$(git -C "$REPO_ROOT" rev-parse --verify --quiet "${ref}^{commit}" 2>/dev/null)"; then
		query="head_sha=$sha"
	fi
	gh api "repos/{owner}/{repo}/actions/runs?$query" \
		--jq ".workflow_runs[] | select(.path == \"$WORKFLOW_PATH\")
			| \"\(.id)\t\(.status)\t\(.conclusion // \"-\")\t\(.html_url)\""
}

# sync_logs mirrors a failed run's detailed logs into a gitignored directory so
# failures can be grepped locally.
sync_logs() {
	local run_id="$1" dest="$LOG_DIR/$1"
	mkdir -p "$dest"
	gh run view "$run_id" --log >"$dest/full.log" 2>&1 || true
	echo "release.sh: synced logs to $dest" >&2
}

# EXPECTED_ASSETS is the set of build artifacts the workflow attaches, kept in
# sync with the target matrix in .github/workflows/build.yml.
EXPECTED_ASSETS=(
	bgx-linux-amd64
	bgx-linux-arm64
	bgx-darwin-arm64
)

# verify_assets fails unless every expected platform build is attached, so a
# partially-successful workflow is never mistaken for a complete release.
verify_assets() {
	local tag="$1" have asset
	have="$(gh release view "$tag" --json assets --jq '.assets[].name')"
	for asset in "${EXPECTED_ASSETS[@]}"; do
		grep -qxF "$asset" <<<"$have" || fail "missing build $asset in release $tag"
	done
}

# smoke_test downloads the build for the current platform and runs it, proving
# the uploaded binary is usable end to end.
smoke_test() {
	local tag="$1" asset tmp
	asset="bgx-$(go env GOOS)-$(go env GOARCH)"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN
	gh release download "$tag" --pattern "$asset" --dir "$tmp"
	chmod +x "$tmp/$asset"
	"$tmp/$asset" version
}

main() {
	local ref_or_tag="" target="" no_promote=false alpha=false list=false
	while [ $# -gt 0 ]; do
		case "$1" in
			--target) shift; target="${1:-}"; [ -n "$target" ] || fail "--target requires a ref" ;;
			--list) list=true ;;
			--no-promote) no_promote=true ;;
			--alpha) alpha=true ;;
			-h|--help) usage; return 0 ;;
			-*) fail "unknown option: $1" ;;
			*) ref_or_tag="$1" ;;
		esac
		shift
	done

	command -v gh >/dev/null 2>&1 || fail "gh cli is required"

	local tag
	if [ -n "$ref_or_tag" ]; then
		tag="$ref_or_tag"
	else
		tag="$(next_minor_tag)"
	fi

	if [ "$list" = true ]; then
		list_runs "$tag"
		return 0
	fi

	# Alpha verification pre-releases are never promoted, and must build the
	# branch under test so unmerged workflow changes are actually exercised.
	if [ "$alpha" = true ]; then
		no_promote=true
		[ -n "$target" ] || target="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
	fi
	[ -n "$target" ] || target="main"

	# Push the tag with git rather than letting `gh release create` create it:
	# tags created through the API do not reliably emit the tag `push` event the
	# workflow triggers on. Pushing also ships the target commit's objects, so an
	# unpushed branch (e.g. under alpha verification) still builds. The
	# pre-release is created right after so the workflow's upload step has a
	# release to attach to long before it finishes building.
	echo "release.sh: tagging $target as $tag and pushing" >&2
	git -C "$REPO_ROOT" tag "$tag" "$target"
	git -C "$REPO_ROOT" push origin "refs/tags/$tag"
	gh release create "$tag" --verify-tag --prerelease --generate-notes --title "$tag"

	local sha
	sha="$(git -C "$REPO_ROOT" rev-parse "$tag^{commit}")"
	echo "release.sh: waiting for the $WORKFLOW workflow to complete" >&2
	local run_id=""
	for _ in $(seq 1 30); do
		run_id="$(run_id_for "$sha")"
		[ -n "$run_id" ] && break
		sleep 5
	done
	[ -n "$run_id" ] || fail "no $WORKFLOW run found for $tag"

	if ! gh run watch "$run_id" --exit-status; then
		sync_logs "$run_id"
		fail "workflow run $run_id failed"
	fi

	verify_assets "$tag"

	if [ "$alpha" = true ]; then
		echo "release.sh: smoke testing the $tag build" >&2
		smoke_test "$tag"
		echo "release.sh: alpha verification for $tag succeeded" >&2
		return 0
	fi

	if [ "$no_promote" = true ]; then
		echo "release.sh: leaving $tag as a pre-release" >&2
		return 0
	fi

	echo "release.sh: promoting $tag to a full release" >&2
	gh release edit "$tag" --prerelease=false --latest
}

main "$@"