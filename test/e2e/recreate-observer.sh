#!/bin/sh
set -eu

old_uid=${OLD_UID:?OLD_UID is required}
ready_file=${READY_FILE:?READY_FILE is required}
summary_file=${SUMMARY_FILE:?SUMMARY_FILE is required}

fail() {
	printf 'error: recreate observation failed: %s\n' "$*" >&2
	exit 2
}

state=initial
new_uid=
samples=0
max_pods=0
zero_observed=false

while IFS= read -r snapshot; do
	count=$(printf '%s\n' "$snapshot" | jq -er 'if type == "array" and all(.[]; type == "string" and length > 0) then length else error("invalid UID snapshot") end') ||
		fail 'snapshot is not a JSON array of non-empty Pod UIDs'
	samples=$((samples + 1))
	[ "$count" -le 1 ] || fail 'more than one matching controller Pod was observed'
	[ "$count" -le "$max_pods" ] || max_pods=$count
	uid=$(printf '%s\n' "$snapshot" | jq -r 'first // ""')
	case "$state:$count" in
		initial:1)
			[ "$uid" = "$old_uid" ] || fail 'the recorded old Pod UID was absent before upgrade'
			state=old
			: >"$ready_file"
			;;
		initial:0) fail 'the first observation did not contain the recorded old Pod UID' ;;
		old:1) [ "$uid" = "$old_uid" ] || fail 'a new Pod UID appeared before the old UID disappeared' ;;
		old:0) state=zero; zero_observed=true ;;
		zero:0) : ;;
		zero:1)
			[ "$uid" != "$old_uid" ] || fail 'the old Pod UID reappeared after Pod zero'
			new_uid=$uid
			state=new
			;;
		new:1) [ "$uid" = "$new_uid" ] || fail 'more than one replacement Pod UID was observed' ;;
		new:0) fail 'the replacement Pod disappeared during observation' ;;
		*) fail 'unexpected observation state' ;;
	esac
done

[ "$state" = new ] || fail 'observation ended before a replacement Pod appeared'
jq -cn --arg old "$old_uid" --arg new "$new_uid" --argjson samples "$samples" \
	--argjson maxPods "$max_pods" --argjson zeroObserved "$zero_observed" \
	'{samples:$samples,maxPods:$maxPods,zeroObserved:$zeroObserved,oldUID:$old,newUID:$new,order:["old","zero","new"]}' >"$summary_file"
