#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
lock=${TOOLS_LOCK:-$repo_root/tools.lock}
install_dir=${CONTROLLER_GEN_INSTALL_DIR:-$HOME/.local/bin}

value() {
	key=$1
	awk -F= -v key="$key" '$1 == key { print substr($0, length(key) + 2); count++ } END { if (count != 1) exit 1 }' "$lock"
}

version=$(value controller-gen.version)
module_sum=$(value controller-gen.module-sum)
checksum=$(value controller-gen.module-zip.sha256)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-controller-gen.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15
archive=$tmp_dir/controller-tools.zip

downloaded_sum=$(go mod download -json "sigs.k8s.io/controller-tools@$version" |
	awk -F'"' '$2 == "Sum" { print $4; count++ } END { if (count != 1) exit 1 }')
[ "$downloaded_sum" = "$module_sum" ] || { echo 'controller-gen Go module sum mismatch' >&2; exit 1; }

curl -fsSLo "$archive" "https://proxy.golang.org/sigs.k8s.io/controller-tools/@v/$version.zip"
actual=$(openssl dgst -sha256 -r "$archive" | awk '{print $1}')
[ "$actual" = "$checksum" ] || { echo 'controller-gen module archive checksum mismatch' >&2; exit 1; }
if [ -x "$install_dir/controller-gen" ] &&
	go version -m "$install_dir/controller-gen" | awk -v version="$version" '$1 == "mod" && $2 == "sigs.k8s.io/controller-tools" && $3 == version { found=1 } END { exit !found }'; then
	"$install_dir/controller-gen" --version
	exit 0
fi
mkdir -p "$install_dir"
GOBIN="$install_dir" go install "sigs.k8s.io/controller-tools/cmd/controller-gen@$version"
go version -m "$install_dir/controller-gen" | awk -v version="$version" '$1 == "mod" && $2 == "sigs.k8s.io/controller-tools" && $3 == version { found=1 } END { exit !found }'
"$install_dir/controller-gen" --version
