#!/usr/bin/env bash
# Live checks through this machine's tailscaled: concurrent public and
# tailnet-only shares, over the tailnet and over Funnel's public ingress,
# a foreground share whose terminal is killed, a hub crash, expiry, a
# password, a record from an earlier boot, and the hub leaving when idle.
# Needs Funnel working (pagir doctor). Usage: scripts/e2e.sh ./pagir
set -uo pipefail

pagir=$(realpath "${1:-./pagir}")
state=${XDG_STATE_HOME:-$HOME/.local/state}/pagir
work=$(mktemp -d)
fails=0
trap '"$pagir" stop all >/dev/null 2>&1; rm -rf "$work"' EXIT

check() { # name expected actual
	if [ "$2" = "$3" ]; then echo "ok    $1"; else echo "FAIL  $1: got '$3', want '$2'"; fails=$((fails + 1)); fi
}
records() { ls "$state" | command grep -vc '^hub.json$'; }
newest() { ls -t "$state"/*.json | command grep -v '/hub.json$' | head -1; }
mounts() { tailscale serve status --json | jq -c '[.Foreground[]?.Web | keys[]] | sort'; }
code() { curl -sS -m 15 "$@" -o /dev/null -w '%{http_code}' 2>/dev/null; }
# A share is gone when its URL is refused: 404 while the hub still serves the
# port for other shares, no connection once the last share on it has ended.
gone() { case $(code "$@") in 404 | 000) echo gone ;; *) echo "still served ($(code "$@"))" ;; esac; }
share() { "$pagir" "$@" >"$work/out" 2>&1 || { echo "FAIL  pagir $*: $(cat "$work/out")"; exit 1; }; jq -r .url "$(newest)"; }

"$pagir" stop all >/dev/null 2>&1
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
dir=$(share -e 10m -u "$work/share")
file=$(share -e 10m "$work/other/report.txt")
tail=$(share -t "$work/share")
check "three shares listed" 3 "$("$pagir" ls 2>/dev/null | tail -n +2 | command grep -c live)"
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
tailtok=$(basename "$tail")
check "tailnet token refused on the public lane" 404 "$(code "https://$host/$tailtok/a.txt")"

# A record from an earlier boot is dropped, never served.
tailid=$(jq -r .id "$(newest)")
jq '.id = "00b007" | .boot = "earlier"' "$(newest)" >"$state/00b007.json"
sleep 2.5
check "earlier boot's record dropped" "" "$(ls "$state" | command grep 00b007)"

# The hub dies hard: the mounts go with it, and systemd brings everything back.
kill -9 "$(jq -r .pid "$state/hub.json")"
sleep 0.5
check "hub crash drops the mounts" "[]" "$(mounts)"
timeout 30 bash -c "until curl -sf -m 3 -o /dev/null '${dir}a.txt'; do sleep 1; done"
check "hub crash: back after restart" 0 $?

"$pagir" stop "$tailid" >/dev/null 2>&1
check "stop: that share is gone" gone "$(gone "${tail}a.txt")"
check "stop: the others stay" hello "$(curl -sS -m 10 "${dir}a.txt")"
check "stop: the idle port is released" '["'"$host"':443"]' "$(mounts)"
"$pagir" stop all >/dev/null 2>&1
check "stop all removes the records" 0 "$(records)"

# A foreground share ends with its terminal, even one killed outright.
"$pagir" -f "$work/share" >"$work/fg.out" 2>"$work/fg.err" &
fg=$!
timeout 60 bash -c "until [ -s '$work/fg.out' ]; do sleep 0.2; done"
fgurl=$(head -1 "$work/fg.out")
check "foreground share works" hello "$(curl -sS -m 10 "${fgurl}a.txt")"
timeout 60 bash -c "until command grep -q 'Requests:' '$work/fg.err'; do sleep 0.2; done"
sleep 1.5
check "foreground share shows requests" 1 "$(command grep -c 'GET /.../a.txt 200' "$work/fg.err")"
kill -9 $fg
sleep 2.5
check "killed terminal ends the share" gone "$(gone "${fgurl}a.txt")"

# A foreground share stopped from elsewhere lets its terminal go.
"$pagir" -f "$work/share" >"$work/fg2.out" 2>"$work/fg2.err" &
fg=$!
timeout 60 bash -c "until command grep -q 'Requests:' '$work/fg2.err'; do sleep 0.2; done"
check "second foreground share started" 0 $?
"$pagir" stop all >/dev/null 2>&1
timeout 10 bash -c "while kill -0 $fg 2>/dev/null; do sleep 0.2; done"
check "stopped foreground share releases its terminal" 0 $?
kill -9 $fg 2>/dev/null

url=$(share -e 50s -p sesame "$work/share/a.txt")
check "password required" 401 "$(code "$url")"
check "password accepted" hello "$(curl -sS -m 10 -u x:sesame "$url")"
timeout 60 bash -c "while [ \$(ls '$state' | command grep -vc '^hub.json\$') -gt 0 ]; do sleep 1; done"
check "expiry ends the share" gone "$(gone -u x:sesame "$url")"
check "expiry removes the record" 0 "$(records)"

timeout 20 bash -c "until ! systemctl --user is-active --quiet pagir.service; do sleep 1; done"
check "idle hub exits" inactive "$(systemctl --user is-active pagir.service)"
check "idle hub leaves no mounts" "[]" "$(mounts)"
check "idle hub removes hub.json" "" "$(ls "$state")"

[ $fails -eq 0 ] && echo "all live checks passed" || { echo "$fails failed"; exit 1; }
