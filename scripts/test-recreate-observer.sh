#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
observer=$repo_root/test/e2e/recreate-observer.sh
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-recreate-observer.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15

deployment_name=controller
deployment_uid=deployment-uid

pod() {
	uid=$1 name=$2 phase=$3 ready=$4 deletion=$5 replica_set_uid=$6 replica_set_name=$7
	jq -cn --arg uid "$uid" --arg name "$name" --arg phase "$phase" --argjson ready "$ready" \
		--argjson deletion "$deletion" --arg rsUID "$replica_set_uid" --arg rsName "$replica_set_name" \
		--arg deploymentName "$deployment_name" --arg deploymentUID "$deployment_uid" '
		{uid:$uid,name:$name,phase:$phase,deletionTimestamp:$deletion,ready:$ready,
		 owner:{podControllerCount:1,podController:{kind:"ReplicaSet",name:$rsName,uid:$rsUID},
			replicaSet:{found:true,name:$rsName,uid:$rsUID,controllerCount:1,
				controller:{kind:"Deployment",name:$deploymentName,uid:$deploymentUID}}}}'
}

snapshot() { printf '%s\n' "$@" | jq -cs '.'; }

old_active=$(pod old old-pod Running true null rs-old old-rs)
old_active_deleting=$(pod old old-pod Running false '"2026-07-13T00:00:00Z"' rs-old old-rs)
old_terminal=$(pod old old-pod Failed false '"2026-07-13T00:00:00Z"' rs-old old-rs)
new_pending=$(pod new new-pod Pending false null rs-new new-rs)
new_unknown=$(pod new new-pod Unknown false null rs-new new-rs)
new_ready=$(pod new new-pod Running true null rs-new new-rs)
other_ready=$(pod other other-pod Running true null rs-other other-rs)
empty='[]'
old_only=$(snapshot "$old_active")
old_terminal_only=$(snapshot "$old_terminal")
terminal_new_pending=$(snapshot "$old_terminal" "$new_pending")
terminal_new_ready=$(snapshot "$old_terminal" "$new_ready")
old_deleting_new_pending=$(snapshot "$old_active_deleting" "$new_pending")
old_deleting_new_unknown=$(snapshot "$old_active_deleting" "$new_unknown")
new_pending_only=$(snapshot "$new_pending")
new_ready_only=$(snapshot "$new_ready")
other_ready_only=$(snapshot "$other_ready")

run() {
	name=$1
	intentional_stop=$2
	shift 2
	stop_file=$tmpdir/$name.stop
	[ "$intentional_stop" != true ] || : >"$stop_file"
	printf '%s\n' "$@" | OLD_UID=old DEPLOYMENT_NAME="$deployment_name" DEPLOYMENT_UID="$deployment_uid" \
		READY_FILE="$tmpdir/$name.ready" SUMMARY_FILE="$tmpdir/$name.json" \
		DIAGNOSTIC_FILE="$tmpdir/$name-diagnostic.json" STOP_FILE="$stop_file" "$observer"
}

run valid-terminal-overlap true "$old_only" "$old_terminal_only" "$terminal_new_pending" "$terminal_new_ready"
jq -e '.samples == 4 and .maxActiveControllers == 1 and .terminalOverlapSamples == 2 and
	.zeroObserved == true and .oldUID == "old" and .newUID == "new" and
	.order == ["old-active","zero-active","new-active-ready"]' "$tmpdir/valid-terminal-overlap.json" >/dev/null
[ -f "$tmpdir/valid-terminal-overlap.ready" ] || {
	printf '%s\n' 'error: valid observation did not signal readiness' >&2
	exit 2
}
[ ! -e "$tmpdir/valid-terminal-overlap-diagnostic.json" ] || {
	printf '%s\n' 'error: valid observation wrote a failure diagnostic' >&2
	exit 2
}

reject() {
	name=$1
	intentional_stop=$2
	shift 2
	if run "$name" "$intentional_stop" "$@" >"$tmpdir/$name.out" 2>"$tmpdir/$name.err"; then
		printf 'error: recreate observer accepted negative fixture: %s\n' "$name" >&2
		exit 2
	fi
	[ ! -e "$tmpdir/$name.json" ] || {
		printf 'error: rejected fixture produced a summary: %s\n' "$name" >&2
		exit 2
	}
	jq -e '
		(.reason | type == "string" and length > 0) and
		(.snapshot | type == "array") and
		all(.snapshot[];
			(keys == ["deletionTimestamp","name","owner","phase","ready","uid"]) and
			(.owner | keys == ["podController","podControllerCount","replicaSet"]))
	' "$tmpdir/$name-diagnostic.json" >/dev/null || {
		printf 'error: rejected fixture lacks a sanitized failure snapshot: %s\n' "$name" >&2
		exit 2
	}
}

reject missing-old true "$empty" "$new_ready_only"
reject invalid-snapshot true 'not-json'
reject pending-overlap true "$old_only" "$old_deleting_new_pending"
reject unknown-overlap true "$old_only" "$old_deleting_new_unknown"
reject new-before-zero true "$old_only" "$new_ready_only"
reject old-reappeared true "$old_only" "$empty" "$old_only"
reject replacement-changed true "$old_only" "$empty" "$new_pending_only" "$other_ready_only"
reject replacement-disappeared true "$old_only" "$empty" "$new_pending_only" "$empty"
reject no-replacement true "$old_only" "$empty"
reject replacement-not-ready true "$old_only" "$empty" "$new_pending_only"
reject premature-eof false "$old_only" "$empty" "$new_ready_only"

wrong_pod_owner=$(printf '%s\n' "$old_active" | jq -c '.owner.podController.kind="Job"')
wrong_deployment_owner=$(printf '%s\n' "$old_active" | jq -c '.owner.replicaSet.controller.uid="other-deployment"')
unsafe_extra_field=$(printf '%s\n' "$old_active" | jq -c '.unexpected="synthetic-sensitive-value"')
reject wrong-pod-owner true "$(snapshot "$wrong_pod_owner")"
reject wrong-deployment-owner true "$(snapshot "$wrong_deployment_owner")"
reject unsafe-extra-field true "$(snapshot "$unsafe_extra_field")"
