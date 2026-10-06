#!/usr/bin/env bash
# BackPack+ — Iran server setup.
#
# Run this on EVERY Iran server, as root. For each BackPack server tunnel that
# goes to your kharej server it adds one forwarded port, the "probe port":
#
#     <probe port>=127.0.0.1:59998
#
# BackPack+ on the kharej server connects to <this server>:<probe port>, the
# tunnel carries that connection back to BackPack+'s echo server on kharej, and
# the reply proves this server and that tunnel work end to end.
#
# Safety:
#   * nothing is changed before you confirm (unless --yes);
#   * every file is backed up, validated with `backpack check -c` and swapped in
#     atomically; tunnels are changed one at a time;
#   * BackPack picks the change up by itself and restarts that one tunnel (its
#     connections drop for ~2 seconds); if the tunnel does not come back with all
#     of its ports, the backup is restored automatically and the script stops.
#
#   bash iran-setup.sh                      # interactive
#   bash iran-setup.sh --kharej 1.2.3.4     # tunnels connected to that kharej
#   bash iran-setup.sh --remove             # take the probe ports out again
#   bash iran-setup.sh --help
set -euo pipefail

VERSION=1

# ---------------------------------------------------------------- toml helpers
# server_ports FILE: print the [server] ports entries, one per line.
# Exit status 3 when the file has no [server] table, 4 when it has no ports key.
server_ports() {
	awk '
	function emit(s,   m) {
		while (match(s, /"[^"]*"/)) { print substr(s, RSTART + 1, RLENGTH - 2); s = substr(s, RSTART + RLENGTH) }
	}
	/^[[:space:]]*\[/ { sec = $0; gsub(/[[:space:]]/, "", sec); inarr = 0 }
	sec == "[server]" { seen = 1 }
	sec == "[server]" && !inarr && /^[[:space:]]*ports[[:space:]]*=[[:space:]]*\[/ {
		found = 1; rest = $0; sub(/^[^[]*\[/, "", rest)
		if (rest ~ /\]/) { sub(/\].*$/, "", rest); emit(rest) } else { inarr = 1; emit(rest) }
		next
	}
	inarr {
		line = $0
		if (line ~ /^[[:space:]]*#/) next
		if (line ~ /\]/) { sub(/\].*$/, "", line); emit(line); inarr = 0; next }
		emit(line)
	}
	END { if (!seen) exit 3; if (!found) exit 4 }
	' "$1"
}

# rewrite_ports FILE ADD REMOVE_SUFFIX: print FILE with the [server] ports array
# rewritten in BackPack'"'"'s own layout, adding entry ADD (if not empty) and dropping
# entries ending in REMOVE_SUFFIX (if not empty). Everything else is copied byte
# for byte. Exit 5 if the array was not found exactly once.
rewrite_ports() {
	awk -v add="$2" -v drop="$3" '
	function collect(s) {
		while (match(s, /"[^"]*"/)) { n++; e[n] = substr(s, RSTART + 1, RLENGTH - 2); s = substr(s, RSTART + RLENGTH) }
	}
	function flush(   i, d) {
		print "ports = ["
		for (i = 1; i <= n; i++) {
			d = drop != "" && length(e[i]) >= length(drop) && substr(e[i], length(e[i]) - length(drop) + 1) == drop
			if (!d) printf "    \"%s\",\n", e[i]
		}
		if (add != "") printf "    \"%s\",\n", add
		print "]"
		done++
	}
	/^[[:space:]]*\[/ && !inarr { sec = $0; gsub(/[[:space:]]/, "", sec) }
	sec == "[server]" && !inarr && /^[[:space:]]*ports[[:space:]]*=[[:space:]]*\[/ {
		n = 0; rest = $0; sub(/^[^[]*\[/, "", rest)
		if (rest ~ /\]/) { sub(/\].*$/, "", rest); collect(rest); flush() } else { inarr = 1; collect(rest) }
		next
	}
	inarr {
		line = $0
		if (line ~ /^[[:space:]]*#/) next
		if (line ~ /\]/) { sub(/\].*$/, "", line); collect(line); inarr = 0; flush(); next }
		collect(line); next
	}
	{ print }
	END { if (done != 1 || inarr) exit 5 }
	' "$1"
}

toml_value() { # toml_value FILE KEY  (first match inside [server])
	awk -v k="$2" '
	/^[[:space:]]*\[/ { sec = $0; gsub(/[[:space:]]/, "", sec) }
	sec == "[server]" && $0 ~ "^[[:space:]]*" k "[[:space:]]*=" {
		v = $0; sub(/^[^=]*=[[:space:]]*/, "", v); gsub(/^"|"[[:space:]]*$/, "", v); print v; exit
	}' "$1"
}

# listen_ports ENTRY: the port numbers an entry listens on ("443", "1.2.3.4:443=x",
# "10000-10009=y" ...), one per line.
listen_ports() {
	local l=${1%%=*}
	l=${l##*:}
	if [[ $l =~ ^([0-9]+)-([0-9]+)$ ]]; then
		local a=${BASH_REMATCH[1]} b=${BASH_REMATCH[2]}
		[ $((b - a)) -le 20000 ] && seq "$a" "$b"
	elif [[ $l =~ ^[0-9]+$ ]]; then
		echo "$l"
	fi
}

listening() { # listening PORT -> 0 if something listens on TCP PORT
	[ "$HAVE_SS" = 1 ] || return 1
	[ -n "$(ss -ltnH "sport = :$1" 2>/dev/null)" ]
}

unit_exists() { command -v systemctl >/dev/null && systemctl cat "backpack-$1.service" >/dev/null 2>&1; }

echo_test() { # echo_test PORT -> 0 when BackPack+ on kharej answered through the tunnel
	local tok="BPP1 iran-setup-$RANDOM$RANDOM"
	timeout 6 bash -c '
		exec 3<>/dev/tcp/127.0.0.1/'"$1"' || exit 1
		printf "%s\n" "$1" >&3
		IFS= read -r -t 5 line <&3 || exit 1
		[ "$line" = "$1" ]' _ "$tok" 2>/dev/null
}

# Sourced by tests/iran-setup-test.sh for the helpers above only.
if [ "${BPP_LIB:-0}" = 1 ]; then return 0 2>/dev/null || exit 0; fi

BACKPACK_DIR=${BACKPACK_DIR:-/etc/backpack}
ECHO_PORT=59998
START_PORT=59999
KHAREJ=""
TUNNELS_ARG=""
ALL=0
YES=0
DRY=0
REMOVE=0
FIREWALL=1
WAIT=20

usage() {
	cat <<EOF
BackPack+ Iran server setup (v$VERSION)

  --kharej IP      the kharej server running BackPack+ (default: asked, or the
                   address the tunnels are connected from)
  --tunnels a,b    only these BackPack tunnels (default: the ones connected to --kharej)
  --all            every server tunnel on this machine
  --port N         first probe port to try (default $START_PORT; then N-2, N-4, ...)
  --echo-port N    BackPack+ echo port on kharej (default $ECHO_PORT)
  --remove         remove the probe ports instead of adding them
  --dry-run        show what would change and change nothing
  --yes            do not ask for confirmation
  --no-firewall    do not touch ufw / firewalld
  --wait N         seconds to wait for a tunnel to come back (default $WAIT)

Environment: BACKPACK_DIR (default /etc/backpack), BACKPACK_BIN (default: backpack in PATH)
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--kharej) KHAREJ=${2:?}; shift ;;
	--tunnels) TUNNELS_ARG=${2:?}; shift ;;
	--all) ALL=1 ;;
	--port) START_PORT=${2:?}; shift ;;
	--echo-port) ECHO_PORT=${2:?}; shift ;;
	--remove) REMOVE=1 ;;
	--dry-run) DRY=1 ;;
	--yes | -y) YES=1 ;;
	--no-firewall) FIREWALL=0 ;;
	--wait) WAIT=${2:?}; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done

if [ -t 1 ]; then
	R=$'\e[31m' G=$'\e[32m' Y=$'\e[33m' B=$'\e[1m' N=$'\e[0m'
else
	R="" G="" Y="" B="" N=""
fi
say() { printf '%s\n' "$*"; }
ok() { printf '  %s✓%s %s\n' "$G" "$N" "$*"; }
warn() { printf '  %s!%s %s\n' "$Y" "$N" "$*"; }
die() { printf '%s✗ %s%s\n' "$R" "$*" "$N" >&2; exit 1; }

ask() { # ask VAR PROMPT — reads the terminal even when stdin is not one
	local _a=""
	if [ -t 0 ]; then
		read -r -p "$2" _a || true
	elif { exec 8</dev/tty; } 2>/dev/null; then
		read -r -p "$2" _a <&8 || true
		exec 8<&-
	else
		printf '%s' "$2"
		read -r _a || true
		echo
	fi
	printf -v "$1" '%s' "$_a"
}
isnum() { [[ $1 =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
isip() { [[ $1 =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; }
isnum "$START_PORT" || die "--port must be 1-65535"
isnum "$ECHO_PORT" || die "--echo-port must be 1-65535"
[[ $WAIT =~ ^[0-9]+$ ]] || die "--wait must be a number"
[ -z "$KHAREJ" ] || isip "$KHAREJ" || die "--kharej must be an IPv4 address"

# ---------------------------------------------------------------- preconditions
[ "${BASH_VERSINFO[0]}" -ge 4 ] || die "bash 4 or newer is required"
[ "$(id -u)" -eq 0 ] || die "run as root"
[ -d "$BACKPACK_DIR" ] || die "$BACKPACK_DIR not found — is BackPack installed on this server?"
BP=${BACKPACK_BIN:-$(command -v backpack || true)}
[ -n "$BP" ] || BP=/usr/local/bin/backpack
[ -x "$BP" ] || die "backpack binary not found (set BACKPACK_BIN)"
command -v awk >/dev/null || die "awk is required"
HAVE_SS=0
command -v ss >/dev/null && HAVE_SS=1

if command -v flock >/dev/null; then
	exec 9>"$BACKPACK_DIR/.backpack-plus.lock"
	flock -n 9 || die "another iran-setup.sh is running"
fi

# ---------------------------------------------------------------- inventory
say "${B}BackPack+ Iran setup${N}  ($("$BP" -v 2>/dev/null | head -1 || echo "backpack ?"), $BACKPACK_DIR)"
declare -a NAMES=() FILES=() TRANSPORTS=() PEERS=() CONNECTED=() PROBES=()
declare -A USED=()
shopt -s nullglob
for f in "$BACKPACK_DIR"/*.toml; do
	name=$(basename "$f" .toml)
	set +e
	entries=$(server_ports "$f")
	rc=$?
	set -e
	# every tunnel's ports are taken, whatever its role
	if [ $rc -eq 3 ]; then
		while IFS= read -r p; do [ -n "$p" ] && USED[$p]=1; done < <(awk '/^[[:space:]]*"[^"]*",?[[:space:]]*$/ { gsub(/[ ",]/, ""); print }' "$f" |
			while IFS= read -r e; do listen_ports "$e"; done)
		continue
	fi
	[ $rc -eq 4 ] && entries=""
	[ $rc -eq 0 ] || [ $rc -eq 4 ] || { warn "$name: could not read, skipped"; continue; }
	probe=""
	while IFS= read -r e; do
		[ -z "$e" ] && continue
		if [[ $e == *"=127.0.0.1:$ECHO_PORT" ]]; then probe=${e%%=*}; probe=${probe##*:}; fi
		while IFS= read -r p; do USED[$p]=1; done < <(listen_ports "$e")
	done <<<"$entries"
	peer="" conn=""
	m="$BACKPACK_DIR/$name.metrics.json"
	if [ -f "$m" ]; then
		peer=$(sed -n 's/.*"peer"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$m" | head -1)
		peer=${peer%:*}
		peer=${peer#[}
		peer=${peer%]}
		grep -q '"connected"[[:space:]]*:[[:space:]]*true' "$m" && conn=1
	fi
	NAMES+=("$name") FILES+=("$f") TRANSPORTS+=("$(toml_value "$f" transport)") PEERS+=("$peer") CONNECTED+=("$conn") PROBES+=("$probe")
done
if [ "$HAVE_SS" = 1 ]; then
	while IFS= read -r p; do [ -n "$p" ] && USED[$p]=1; done < <(ss -ltnH 2>/dev/null | awk '{ n = split($4, a, ":"); print a[n] }')
fi

[ ${#NAMES[@]} -gt 0 ] || die "no BackPack server tunnels in $BACKPACK_DIR — this script is for the Iran (server) side"

say ""
printf "  %-3s %-16s %-10s %-24s %s\n" "#" "tunnel" "transport" "connected from" "probe port"
for i in "${!NAMES[@]}"; do
	st="${PEERS[$i]:-?}"
	[ -n "${CONNECTED[$i]}" ] && st="$st (online)"
	printf "  %-3s %-16s %-10s %-24s %s\n" "$((i + 1))" "${NAMES[$i]}" "${TRANSPORTS[$i]}" "$st" "${PROBES[$i]:--}"
done
say ""

# ---------------------------------------------------------------- selection
if [ -z "$KHAREJ" ] && [ -z "$TUNNELS_ARG" ] && [ "$ALL" = 0 ]; then
	# most common peer is the likely kharej
	guess=$(printf '%s\n' "${PEERS[@]}" | sed '/^$/d' | sort | uniq -c | sort -rn | awk 'NR==1 {print $2}')
	if [ "$YES" = 1 ]; then
		KHAREJ=$guess
	else
		ask KHAREJ "IP of the kharej server running BackPack+ [${guess}]: "
		KHAREJ=${KHAREJ:-$guess}
	fi
	[ -n "$KHAREJ" ] || die "the kharej IP is needed (use --kharej)"
	isip "$KHAREJ" || die "$KHAREJ is not an IPv4 address"
fi

declare -a SEL=()
if [ -n "$TUNNELS_ARG" ]; then
	IFS=',' read -r -a want <<<"$TUNNELS_ARG"
	for w in "${want[@]}"; do
		w=$(echo "$w" | tr -d ' ')
		found=""
		for i in "${!NAMES[@]}"; do [ "${NAMES[$i]}" = "$w" ] && found=$i; done
		[ -n "$found" ] || die "no server tunnel named '$w'"
		SEL+=("$found")
	done
elif [ "$ALL" = 1 ]; then
	SEL=("${!NAMES[@]}")
elif [ "$REMOVE" = 1 ] && [ -z "$KHAREJ" ]; then
	for i in "${!NAMES[@]}"; do [ -n "${PROBES[$i]}" ] && SEL+=("$i"); done
else
	for i in "${!NAMES[@]}"; do [ "${PEERS[$i]}" = "$KHAREJ" ] && SEL+=("$i"); done
	if [ ${#SEL[@]} -eq 0 ]; then
		warn "no tunnel is currently connected from $KHAREJ"
		[ "$YES" = 0 ] || die "nothing selected (use --tunnels or --all)"
	fi
	if [ "$YES" = 0 ]; then
		def=""
		for i in "${SEL[@]}"; do def="$def${def:+,}$((i + 1))"; done
		ask pick "Tunnels to the kharej BackPack+ runs on (numbers, comma separated) [$def]: "
		pick=${pick:-$def}
		SEL=()
		IFS=',' read -r -a nums <<<"$pick"
		for x in "${nums[@]}"; do
			x=$(echo "$x" | tr -d ' ')
			[[ $x =~ ^[0-9]+$ ]] && [ "$x" -ge 1 ] && [ "$x" -le ${#NAMES[@]} ] || die "'$x' is not one of the numbers above"
			SEL+=("$((x - 1))")
		done
	fi
fi
[ ${#SEL[@]} -gt 0 ] || die "no tunnels selected"
# de-duplicate, keep order
declare -A seen=()
declare -a S2=()
for i in "${SEL[@]}"; do [ -z "${seen[$i]:-}" ] && S2+=("$i") && seen[$i]=1; done
SEL=("${S2[@]}")

if [ -z "$KHAREJ" ]; then
	peers=$(for i in "${SEL[@]}"; do echo "${PEERS[$i]}"; done | sed '/^$/d' | sort -u)
	if [ -n "$peers" ] && [ "$(printf '%s\n' "$peers" | wc -l)" -eq 1 ]; then KHAREJ=$peers; fi
fi

# ---------------------------------------------------------------- plan
declare -A NEWPORT=()
next=$START_PORT
PICKED=""
pick_port() { # sets PICKED; not called in a subshell so USED and next stick
	while [ "$next" -ge 1024 ]; do
		local p=$next
		next=$((next - 2))
		[ "$p" = "$ECHO_PORT" ] && continue
		[ -n "${USED[$p]:-}" ] && continue
		USED[$p]=1
		PICKED=$p
		return 0
	done
	return 1
}
say "${B}Plan${N}"
CHANGES=0
for i in "${SEL[@]}"; do
	if [ "$REMOVE" = 1 ]; then
		if [ -n "${PROBES[$i]}" ]; then
			say "  ${NAMES[$i]}: remove probe port ${PROBES[$i]}"
			CHANGES=$((CHANGES + 1))
		else
			say "  ${NAMES[$i]}: has no probe port, nothing to do"
		fi
	elif [ -n "${PROBES[$i]}" ]; then
		say "  ${NAMES[$i]}: already has probe port ${PROBES[$i]} — unchanged"
	else
		pick_port || die "no free port found below $START_PORT"
		p=$PICKED
		NEWPORT[$i]=$p
		say "  ${NAMES[$i]}: add  \"$p=127.0.0.1:$ECHO_PORT\""
		CHANGES=$((CHANGES + 1))
	fi
done
if [ "$CHANGES" -gt 0 ] && [ "$DRY" = 0 ]; then
	say ""
	warn "each changed tunnel restarts itself once: its connections drop for about 2 seconds."
fi
if [ "$DRY" = 1 ]; then
	say ""
	say "dry run — nothing changed."
fi
if [ "$CHANGES" -gt 0 ] && [ "$DRY" = 0 ] && [ "$YES" = 0 ]; then
	ask a "Apply? [y/N]: "
	[[ $a =~ ^[Yy] ]] || die "cancelled, nothing changed"
fi

# ---------------------------------------------------------------- apply
BACKUPS="$BACKPACK_DIR/backpack-plus-backups"
apply_one() { # apply_one INDEX
	local i=$1 name=${NAMES[$1]} f=${FILES[$1]} add="" drop="" port
	if [ "$REMOVE" = 1 ]; then
		port=${PROBES[$i]}
		drop="=127.0.0.1:$ECHO_PORT"
	else
		port=${NEWPORT[$i]}
		add="$port=127.0.0.1:$ECHO_PORT"
	fi
	local tmp="$BACKPACK_DIR/.$name.toml.bpp-new" bak
	mkdir -p "$BACKUPS"
	bak="$BACKUPS/$name.toml.$(date +%Y%m%d-%H%M%S)"
	cp -p "$f" "$bak"
	cp -p "$f" "$tmp"
	if ! rewrite_ports "$f" "$add" "$drop" >"$tmp"; then
		rm -f "$tmp"
		die "$name: could not find the ports list in $f; nothing changed"
	fi

	# The rewritten list must be exactly the old one plus/minus the probe entry.
	local before after expect
	before=$(server_ports "$f" | grep -v -- "=127.0.0.1:$ECHO_PORT\$" || true)
	after=$(server_ports "$tmp" || true)
	if [ "$REMOVE" = 1 ]; then expect=$before; else expect=$(printf '%s\n%s' "$(server_ports "$f")" "$add" | sed '/^$/d'); fi
	if [ "$after" != "$expect" ]; then
		rm -f "$tmp"
		die "$name: the rewritten ports list is not what was expected; nothing changed"
	fi
	# Outside the ports list the file must be byte-identical.
	if ! diff <(awk '/^[[:space:]]*ports[[:space:]]*=/{skip=1} !skip{print} skip && /\]/{skip=0}' "$f") \
		<(awk '/^[[:space:]]*ports[[:space:]]*=/{skip=1} !skip{print} skip && /\]/{skip=0}' "$tmp") >/dev/null; then
		rm -f "$tmp"
		die "$name: something besides the ports list would change; nothing changed"
	fi
	if ! out=$("$BP" check -c "$tmp" 2>&1); then
		rm -f "$tmp"
		die "$name: backpack rejected the new file, nothing changed:
$out"
	fi

	# Ports this tunnel serves right now; they must all be served again afterwards.
	local -a live=()
	while IFS= read -r e; do
		[ -z "$e" ] && continue
		[[ $e == *"=127.0.0.1:$ECHO_PORT" ]] && continue
		while IFS= read -r p; do listening "$p" && live+=("$p"); done < <(listen_ports "$e")
	done <<<"$(server_ports "$f")"

	mv -f "$tmp" "$f"
	say "  $name: saved (backup: $bak), waiting for the tunnel to reload ..."

	reloaded() {
		if [ "$REMOVE" = 1 ]; then ! listening "$port"; else listening "$port"; fi
	}
	others_up() {
		local p
		for p in ${live[@]+"${live[@]}"}; do listening "$p" || return 1; done
		return 0
	}
	wait_for() { # wait_for SECONDS
		local t=0
		while [ "$t" -lt "$1" ]; do
			if reloaded && others_up; then return 0; fi
			sleep 1
			t=$((t + 1))
		done
		return 1
	}

	if [ "$HAVE_SS" = 0 ]; then
		warn "$name: 'ss' is not installed, cannot confirm the reload — check the tunnel yourself"
		return 0
	fi
	if wait_for "$WAIT"; then
		ok "$name: reloaded, all ports listening"
	else
		if unit_exists "$name"; then
			warn "$name: no reload seen, restarting backpack-$name.service"
			systemctl restart "backpack-$name.service" || true
		fi
		if ! wait_for "$WAIT"; then
			warn "$name: the tunnel did not come back with its ports — restoring the backup"
			cp -p "$bak" "$f"
			unit_exists "$name" && systemctl restart "backpack-$name.service" || true
			sleep 3
			die "$name: restored $bak. Check it with: backpack tunnel status $name"
		fi
		ok "$name: restarted, all ports listening"
	fi

	if [ "$REMOVE" = 0 ]; then
		local t=0 answered=0
		while [ "$t" -lt 4 ]; do
			if echo_test "$port"; then answered=1; break; fi
			sleep 3
			t=$((t + 1))
		done
		if [ "$answered" = 1 ]; then
			ok "$name: probe port $port answers from BackPack+ on kharej — end to end OK"
		else
			warn "$name: no answer through port $port yet (normal if BackPack+ is not running on kharej yet)"
		fi
	fi
}

firewall() { # firewall add|remove PORT
	[ "$FIREWALL" = 1 ] || return 0
	local act=$1 p=$2
	if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
		if [ -z "$KHAREJ" ]; then
			warn "ufw is active but the kharej IP is unknown: allow TCP $p from it yourself"
			return 0
		fi
		if [ "$act" = add ]; then
			ufw allow proto tcp from "$KHAREJ" to any port "$p" comment 'backpack-plus probe' >/dev/null && ok "ufw: allowed TCP $p from $KHAREJ"
		else
			ufw delete allow proto tcp from "$KHAREJ" to any port "$p" >/dev/null 2>&1 && ok "ufw: removed rule for TCP $p" || true
		fi
	elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
		if [ -z "$KHAREJ" ]; then
			warn "firewalld is running but the kharej IP is unknown: allow TCP $p from it yourself"
			return 0
		fi
		local rule="rule family=ipv4 source address=$KHAREJ port port=$p protocol=tcp accept"
		if [ "$act" = add ]; then
			firewall-cmd --permanent --add-rich-rule="$rule" >/dev/null && firewall-cmd --add-rich-rule="$rule" >/dev/null && ok "firewalld: allowed TCP $p from $KHAREJ"
		else
			firewall-cmd --permanent --remove-rich-rule="$rule" >/dev/null 2>&1 || true
			firewall-cmd --remove-rich-rule="$rule" >/dev/null 2>&1 || true
		fi
	elif [ "$act" = add ] && command -v iptables >/dev/null && iptables -S INPUT 2>/dev/null | head -1 | grep -q -- "-P INPUT DROP"; then
		warn "iptables INPUT policy is DROP: allow TCP $p from ${KHAREJ:-the kharej server} yourself"
	fi
}

if [ "$DRY" = 0 ]; then
	[ "$CHANGES" -gt 0 ] && say "" && say "${B}Applying${N}"
	for i in "${SEL[@]}"; do
		if [ "$REMOVE" = 1 ]; then
			[ -n "${PROBES[$i]}" ] || continue
			apply_one "$i"
			firewall remove "${PROBES[$i]}"
			PROBES[$i]=""
		elif [ -n "${NEWPORT[$i]:-}" ]; then
			apply_one "$i"
			firewall add "${NEWPORT[$i]}"
			PROBES[$i]=${NEWPORT[$i]}
		else
			firewall add "${PROBES[$i]}"
			if echo_test "${PROBES[$i]}"; then
				ok "${NAMES[$i]}: probe port ${PROBES[$i]} answers from BackPack+ on kharej — end to end OK"
			else
				warn "${NAMES[$i]}: no answer through probe port ${PROBES[$i]} (is BackPack+ running on kharej?)"
			fi
		fi
	done
fi

# ---------------------------------------------------------------- summary
if [ "$REMOVE" = 0 ]; then
	line=""
	for i in "${SEL[@]}"; do
		p=${PROBES[$i]:-${NEWPORT[$i]:-}}
		[ -n "$p" ] && line="$line${line:+,}$p:${NAMES[$i]}"
	done
	say ""
	if [ "$DRY" = 1 ]; then
		say "${B}After a real run${N}, the kharej server would get these probe ports for this server:"
	else
		say "${B}For the kharej server${N} — in 'backpack-plus setup', give this server these probe ports:"
	fi
	say ""
	say "    $line"
	say ""
	ips=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.' | grep -vE '^(10|127|172\.(1[6-9]|2[0-9]|3[01])|192\.168)\.' | head -3 | tr '\n' ' ' || true)
	[ -n "$ips" ] && say "  (this server's public IPv4 address(es): $ips)"
fi
exit 0
