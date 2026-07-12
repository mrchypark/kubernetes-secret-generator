#!/bin/sh
set -eu

[ "$#" -eq 1 ] || { printf '%s\n' 'usage: check-coverage.sh PROFILE' >&2; exit 2; }
[ -f "$1" ] && [ ! -L "$1" ] || { printf '%s\n' 'coverage profile must be a regular non-symlink file' >&2; exit 2; }

awk '
BEGIN {
  critical[1]="pkg/controller/crd"
  critical[2]="pkg/controller/crd/stringsecret"
  critical[3]="pkg/controller/crd/basicauth"
  critical[4]="pkg/controller/crd/sshkeypair"
  critical[5]="pkg/controller/secret"
  critical[6]="pkg/apis/secretgenerator/v1alpha1"
}
NR == 1 {
  if ($0 != "mode: set") bad=1
  next
}
{
  if (NF != 3 || $2 !~ /^[0-9]+$/ || $3 !~ /^[0-9]+$/) { bad=1; next }
  file=$1
  sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
  if (file ~ /\/zz_generated\.deepcopy\.go$/) next
  statements=$2+0
  total+=statements
  if ($3+0 > 0) covered+=statements
  package=file
  sub(/^.*\/pkg\//, "pkg/", package)
  sub("/[^/]+$", "", package)
  package_total[package]+=statements
  if ($3+0 > 0) package_covered[package]+=statements
}
END {
  if (bad || NR < 2 || total == 0) {
    print "invalid or empty coverage profile" > "/dev/stderr"
    exit 2
  }
  ok=(covered*100 >= total*80)
  printf "overall %.1f%% (required 80%%)\n", covered*100/total
  for (i=1; i<=6; i++) {
    package=critical[i]
    package_ok=(package_total[package] > 0 && package_covered[package]*100 >= package_total[package]*90)
    if (!package_ok) ok=0
    percent=package_total[package] ? package_covered[package]*100/package_total[package] : 0
    printf "%s %.1f%% (required 90%%)\n", package, percent
  }
  if (!ok) exit 1
}
' "$1"
