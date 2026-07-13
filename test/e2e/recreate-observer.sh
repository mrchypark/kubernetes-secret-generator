#!/bin/sh
set -eu

old_uid=${OLD_UID:?OLD_UID is required}
deployment_name=${DEPLOYMENT_NAME:?DEPLOYMENT_NAME is required}
deployment_uid=${DEPLOYMENT_UID:?DEPLOYMENT_UID is required}
ready_file=${READY_FILE:?READY_FILE is required}
summary_file=${SUMMARY_FILE:?SUMMARY_FILE is required}
diagnostic_file=${DIAGNOSTIC_FILE:?DIAGNOSTIC_FILE is required}
stop_file=${STOP_FILE:?STOP_FILE is required}

last_snapshot='[]'

fail() {
	reason=$*
	umask 077
	if ! jq -cn --arg reason "$reason" --argjson snapshot "$last_snapshot" \
		'{reason:$reason,snapshot:$snapshot}' >"$diagnostic_file"; then
		printf '%s\n' 'error: recreate observation failed and its sanitized diagnostic could not be written' >&2
	fi
	printf 'error: recreate observation failed: %s\n' "$reason" >&2
	exit 2
}

state=initial
new_uid=
samples=0
max_active_controllers=0
terminal_overlap_samples=0
zero_observed=false

while IFS= read -r raw_snapshot; do
	if ! snapshot=$(printf '%s\n' "$raw_snapshot" | jq -ceS '
		if type != "array" then error("snapshot is not an array") else . end |
		if all(.[].uid; type == "string" and length > 0) and
		   all(.[].name; type == "string" and length > 0) and
		   all(.[].phase; type == "string" and IN("Pending","Running","Succeeded","Failed","Unknown")) and
		   all(.[].deletionTimestamp; . == null or (type == "string" and length > 0)) and
		   all(.[].ready; type == "boolean") and
		   all(.[];
			(keys == ["deletionTimestamp","name","owner","phase","ready","uid"]) and
			(.owner | keys == ["podController","podControllerCount","replicaSet"]) and
			(.owner.podController | type == "object" and ((keys - ["kind","name","uid"]) | length == 0)) and
			(.owner.replicaSet | keys == ["controller","controllerCount","found","name","uid"]) and
			(.owner.replicaSet.controller | type == "object" and ((keys - ["kind","name","uid"]) | length == 0)))
		then . else error("snapshot has invalid Pod identity or status fields") end |
		if ([.[].uid] | unique | length) == length then . else error("snapshot has duplicate Pod UIDs") end
	'); then
		last_snapshot='[]'
		fail 'snapshot is not a sanitized Pod status array'
	fi
	last_snapshot=$snapshot
	samples=$((samples + 1))

	if ! printf '%s\n' "$snapshot" | jq -e --arg deploymentName "$deployment_name" --arg deploymentUID "$deployment_uid" '
		all(.[];
			.owner.podControllerCount == 1 and
			.owner.podController.kind == "ReplicaSet" and
			(.owner.podController.name | type == "string" and length > 0) and
			(.owner.podController.uid | type == "string" and length > 0) and
			.owner.replicaSet.found == true and
			.owner.replicaSet.name == .owner.podController.name and
			.owner.replicaSet.uid == .owner.podController.uid and
			.owner.replicaSet.controllerCount == 1 and
			.owner.replicaSet.controller.kind == "Deployment" and
			.owner.replicaSet.controller.name == $deploymentName and
			.owner.replicaSet.controller.uid == $deploymentUID)
	' >/dev/null; then
		fail 'a matching Pod did not have the exact controller Deployment owner chain'
	fi

	active=$(printf '%s\n' "$snapshot" | jq -c '[.[] | select(.phase != "Succeeded" and .phase != "Failed")]')
	active_count=$(printf '%s\n' "$active" | jq 'length')
	[ "$active_count" -le 1 ] || fail 'more than one exact controller Deployment-owned active Pod was observed'
	[ "$active_count" -le "$max_active_controllers" ] || max_active_controllers=$active_count
	if printf '%s\n' "$snapshot" | jq -e '
		(length > 1) and
		any(.[]; .phase == "Succeeded" or .phase == "Failed") and
		any(.[]; .phase != "Succeeded" and .phase != "Failed")
	' >/dev/null; then
		terminal_overlap_samples=$((terminal_overlap_samples + 1))
	fi
	active_uid=$(printf '%s\n' "$active" | jq -r 'first.uid // ""')
	active_ready=$(printf '%s\n' "$active" | jq -r 'first.ready // false')

	case "$state:$active_count" in
		initial:1)
			[ "$active_uid" = "$old_uid" ] || fail 'the recorded old active Pod UID was absent before upgrade'
			state=old
			: >"$ready_file"
			;;
		initial:0) fail 'the first observation did not contain the recorded old active Pod UID' ;;
		old:1) [ "$active_uid" = "$old_uid" ] || fail 'a replacement Pod became active before Pod zero' ;;
		old:0) state=zero; zero_observed=true ;;
		zero:0) : ;;
		zero:1)
			[ "$active_uid" != "$old_uid" ] || fail 'the old Pod UID became active again after Pod zero'
			new_uid=$active_uid
			if [ "$active_ready" = true ]; then state=new; else state=starting; fi
			;;
		starting:1)
			[ "$active_uid" = "$new_uid" ] || fail 'the replacement active Pod UID changed before Ready'
			[ "$active_ready" != true ] || state=new
			;;
		starting:0) fail 'the replacement Pod stopped being active before Ready' ;;
		new:1)
			[ "$active_uid" = "$new_uid" ] || fail 'the Ready replacement active Pod UID changed'
			[ "$active_ready" = true ] || fail 'the replacement Pod lost Ready during observation'
			;;
		new:0) fail 'the Ready replacement Pod stopped being active during observation' ;;
		*) fail 'unexpected observation state' ;;
	esac
done

[ -e "$stop_file" ] || fail 'observation input ended without the intentional stop handshake'
[ "$state" = new ] || fail 'observation ended before a replacement active Pod became Ready'
jq -cn --arg old "$old_uid" --arg new "$new_uid" --argjson samples "$samples" \
	--argjson maxActiveControllers "$max_active_controllers" --argjson terminalOverlapSamples "$terminal_overlap_samples" \
	--argjson zeroObserved "$zero_observed" \
	'{samples:$samples,maxActiveControllers:$maxActiveControllers,terminalOverlapSamples:$terminalOverlapSamples,zeroObserved:$zeroObserved,oldUID:$old,newUID:$new,order:["old-active","zero-active","new-active-ready"]}' >"$summary_file"
