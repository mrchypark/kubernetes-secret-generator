#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
install_dir=${CONTROLLER_GEN_INSTALL_DIR:-$repo_root/.cache/tools}
[ -x "$install_dir/controller-gen" ] || { echo 'verified controller-gen binary is required' >&2; exit 2; }

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-controller-gen-lock-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15
for field in module-sum module-zip.sha256; do
	sed "s|^controller-gen.$field=.*|controller-gen.$field=invalid|" "$repo_root/tools.lock" >"$tmp_dir/tools.lock"
	if TOOLS_LOCK="$tmp_dir/tools.lock" CONTROLLER_GEN_INSTALL_DIR="$install_dir" "$repo_root/scripts/install-controller-gen.sh" >/dev/null 2>&1; then
		echo "controller-gen installer reused a binary without verifying locked $field" >&2
		exit 1
	fi
done

echo 'controller-gen lock bypass test passed'
