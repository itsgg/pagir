#!/usr/bin/env bash
# Live checks through this machine's tailscaled: concurrent public and
# tailnet-only shares over the tailnet and over Funnel's public ingress,
# foreground shares killed and stopped, expiry, a password, a record from an
# earlier boot, the warm funnel, a hub crash and pagir stop hub.
# Needs Funnel working (pagir doctor). Usage: scripts/e2e.sh ./pagir
#
# Shares this script did not make are never stopped. While any exist, the
# checks that interrupt every share (the crash and stop hub) are skipped.
set -uo pipefail

pagir=$(realpath "${1:-./pagir}")
state=${XDG_STATE_HOME:-$HOME/.local/state}/pagir
work=$(mktemp -d)
fails=0
mine=()
cleanup() {
	[ ${#mine[@]} -gt 0 ] && "$pagir" stop "${mine[@]}" >/dev/null 2>&1
	rm -rf "$work"
}
trap cleanup EXIT

check() { # name expected actual
	if [ "$2" = "$3" ]; then echo "ok    $1"; else echo "FAIL  $1: got '$3', want '$2'"; fails=$((fails + 1)); fi
}
ids() { ls "$state" 2>/dev/null | sed -n 's/^\([0-9a-f]\{6\}\)\.json$/\1/p'; }
mounts() { tailscale serve status --json | jq -c '[.Foreground[]?.Web | keys[]] | sort'; }
code() { curl -sS -m 15 "$@" -o /dev/null -w '%{http_code}' 2>/dev/null; }
# A share is gone when its URL is refused: 404 while the hub still serves
# the port, no connection once nothing does.
gone() { case $(code "$@") in 404 | 000) echo gone ;; *) echo "still served ($(code "$@"))" ;; esac; }
# share ARGS... runs pagir, remembers the new share in mine, and leaves its
# URL in $URL (not printed: a $(...) subshell would lose mine); its output
# stays in $work/out and $work/err for checks.
share() {
	local before after id
	before=$(ids)
	"$pagir" "$@" >"$work/out" 2>"$work/err" || { echo "FAIL  pagir $*: $(cat "$work/err")"; exit 1; }
	after=$(ids)
	id=$(comm -13 <(echo "$before" | sort) <(echo "$after" | sort) | head -1)
	[ -n "$id" ] && mine+=("$id")
	URL=$(head -1 "$work/out")
}
idof() { basename "$(command grep -l "\"url\": \"$1\"" "$state"/*.json 2>/dev/null | head -1)" .json; }

foreign=$(ids | wc -l)
[ "$foreign" -gt 0 ] && echo "note  $foreign share(s) made elsewhere stay up; the crash and stop-hub checks are skipped"
host=$(tailscale status --json | jq -r '.Self.DNSName | rtrimstr(".")')
ip=$(curl -sS -m 8 "https://dns.google/resolve?name=$host&type=A" | jq -r '.Answer[0].data // empty')
[ -n "$ip" ] || { echo "no public A record for $host: run pagir doctor"; exit 1; }
public=(--resolve "$host:443:$ip" --resolve "$host:8443:$ip")

mkdir -p "$work/share/sub" "$work/other"
echo hello >"$work/share/a.txt"
echo inner >"$work/share/sub/b.txt"
echo TOKEN=1 >"$work/share/.env"
echo report >"$work/other/report.txt"

# Three shares at once: a folder and a file through Funnel, a folder on the tailnet only.
share -e 10m -u "$work/share"; dir=$URL
share -e 10m "$work/other/report.txt"; file=$URL
share -t "$work/share"; tail=$URL
check "three shares listed live" 3 "$("$pagir" ls 2>/dev/null | command grep -cE "^($(IFS='|'; echo "${mine[*]}")) +live ")"
check "one listener per port" '["'"$host"':443","'"$host"':8443"]' "$(mounts)"
for via in tailnet public; do
	opts=()
	[ $via = public ] && opts=("${public[@]}")
	check "$via: file in folder" hello "$(curl -sS -m 20 "${opts[@]}" "${dir}a.txt")"
	check "$via: nested file" inner "$(curl -sS -m 20 "${opts[@]}" "${dir}sub/b.txt")"
	check "$via: single-file share" report "$(curl -sS -m 20 "${opts[@]}" "$file")"
	check "$via: dotfile refused" 404 "$(code "${opts[@]}" "${dir}.env")"
	check "$via: host root refused" 404 "$(code "${opts[@]}" "https://$host/")"
	check "$via: unknown token refused" 404 "$(code "${opts[@]}" "https://$host/nosuchtoken/")"
	curl -sS -m 20 "${opts[@]}" -o "$work/$via.zip" "${dir}?zip"
	check "$via: zip holds the tree" "share/a.txt share/sub/b.txt" "$(unzip -Z1 "$work/$via.zip" | command grep -v '^share/up-' | sort | paste -sd' ')"
	check "$via: upload" "up-$via.txt" "$(echo up | curl -sS -m 20 "${opts[@]}" -T - "${dir}up-$via.txt")"
done
check "tailnet-only: tailnet" hello "$(curl -sS -m 10 "${tail}a.txt")"
check "tailnet-only: public refused" 000 "$(code "${public[@]}" "${tail}a.txt")"
check "tailnet token refused on the public lane" 404 "$(code "https://$host/$(basename "$tail")/a.txt")"
"$pagir" log "$(idof "$dir")" >"$work/log" 2>&1
check "the share's log has its requests" 1 "$(( $(command grep -c ' GET ' "$work/log") > 0 ? 1 : 0 ))"
check "the hub's public checks stay out of the log" 0 "$(command grep -c ' HEAD ' "$work/log")"

# On a warm funnel a new share is public, every address checked, at once.
t0=$(date +%s)
share -e 10m "$work/other/report.txt"
check "warm share says public" 1 "$(command grep -c ', public,' "$work/err")"
check "warm share is quick" 1 "$(( $(date +%s) - t0 < 6 ? 1 : 0 ))"
check "warm share prints two lines" 2 "$(( $(wc -l <"$work/out") + $(wc -l <"$work/err") ))"

# A record from before the last boot is dropped, never served.
jq '.id = "00b007" | .created = "2000-01-01T00:00:00Z"' "$state/$(idof "$file").json" >"$state/00b007.json"
sleep 2.5
check "earlier boot's record dropped" "" "$(ls "$state" | command grep 00b007)"

tailid=$(idof "$tail")
"$pagir" stop "$tailid" >/dev/null 2>&1
check "stop: that share is gone" gone "$(gone "${tail}a.txt")"
check "stop: the others stay" hello "$(curl -sS -m 10 "${dir}a.txt")"
check "stop: the idle tailnet port is released" '["'"$host"':443"]' "$(mounts)"

# A foreground share ends with its terminal, even one killed outright.
"$pagir" -f "$work/share" >"$work/fg.out" 2>"$work/fg.err" &
fg=$!
timeout 60 bash -c "until command grep -q 'Requests:' '$work/fg.err'; do sleep 0.2; done"
check "foreground share started" 0 $?
fgurl=$(head -1 "$work/fg.out")
check "foreground share works" hello "$(curl -sS -m 10 "${fgurl}a.txt")"
sleep 1.5
check "foreground share shows requests" 1 "$(command grep -c 'GET /.../a.txt 200' "$work/fg.err")"
kill -9 $fg
wait $fg 2>/dev/null
sleep 13
check "killed terminal ends the share" gone "$(gone "${fgurl}a.txt")"

# A foreground share stopped from elsewhere lets its terminal go.
"$pagir" -f "$work/share" >"$work/fg2.out" 2>"$work/fg2.err" &
fg=$!
timeout 60 bash -c "until command grep -q 'Requests:' '$work/fg2.err'; do sleep 0.2; done"
check "second foreground share started" 0 $?
"$pagir" stop "$(idof "$(head -1 "$work/fg2.out")")" >/dev/null 2>&1
timeout 10 bash -c "while kill -0 $fg 2>/dev/null; do sleep 0.2; done"
check "stopped foreground share releases its terminal" 0 $?
kill -9 $fg 2>/dev/null

share -e 30s -p sesame "$work/share/a.txt"; url=$URL
pwid=$(idof "$url")
check "password required" 401 "$(code "$url")"
check "password accepted" hello "$(curl -sS -m 10 -u x:sesame "$url")"
timeout 45 bash -c "while [ -e '$state/$pwid.json' ]; do sleep 1; done"
check "expiry ends the share" gone "$(gone -u x:sesame "$url")"

"$pagir" stop "${mine[@]}" >/dev/null 2>&1
mine=()
# Count again: a share made elsewhere during the run must not be interrupted.
foreign=$(ids | wc -l)
if [ "$foreign" -eq 0 ]; then
	check "the hub keeps the funnel warm with no shares" 1 "$(mounts | command grep -c "$host:443")"
	share -e 10m "$work/share"; dir=$URL
	kill -9 "$(jq -r .pid "$state/hub.json")"
	sleep 1
	check "hub crash drops the mount" "[]" "$(mounts)"
	timeout 30 bash -c "until curl -sf -m 3 -o /dev/null '${dir}a.txt'; do sleep 1; done"
	check "hub crash: the supervisor brings it back" 0 $?
	"$pagir" stop hub >"$work/out" 2>&1
	mine=()
	check "stop hub frees port 443" 1 "$(command grep -c 'port 443 is free' "$work/out")"
	check "stop hub leaves no mounts" "[]" "$(mounts)"
	check "stop hub leaves no state but the log and lock" "hub.lock hub.log" "$(ls "$state" | command grep -v '^hub.log.1$' | paste -sd' ')"
else
	echo "skip  warm funnel, hub crash and stop hub: $foreign share(s) made elsewhere would be interrupted or would hold the funnel up"
fi

[ $fails -eq 0 ] && echo "all live checks passed" || { echo "$fails failed"; exit 1; }
