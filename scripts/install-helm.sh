#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
lock=$repo_root/tools.lock
install_dir=${HELM_INSTALL_DIR:-$HOME/.local/bin}

value() {
	key=$1
	awk -F= -v key="$key" '$1 == key { print $2; count++ } END { if (count != 1) exit 1 }' "$lock"
}

case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) echo 'unsupported Helm host OS' >&2; exit 1 ;;
esac
case $(uname -m) in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) echo 'unsupported Helm host architecture' >&2; exit 1 ;;
esac

version=$(value helm.version)
checksum=$(value "helm.$os-$arch.sha256")
archive="helm-$version-$os-$arch.tar.gz"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-helm.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15

curl -fsSLo "$tmp_dir/$archive" "https://get.helm.sh/$archive"
actual=$(openssl dgst -sha256 -r "$tmp_dir/$archive" | awk '{print $1}')
[ "$actual" = "$checksum" ] || { echo 'Helm archive checksum mismatch' >&2; exit 1; }
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
mkdir -p "$install_dir"
install -m 0755 "$tmp_dir/$os-$arch/helm" "$install_dir/helm"
"$install_dir/helm" version --short
