#!/bin/bash
#
# fetch-samples.sh downloads the sample masters used in the "Video
# Encoding for the Web in 2026" posts from scottstuff.net, about 98 GB
# in all.
#
#   ./fetch-samples.sh [-n] [-d dir] [name ...]
#
#   -n      list what would be downloaded, and how much, without
#           downloading anything
#   -d dir  where to put them (default: samples); each master lands at
#           dir/sample-<name>/<file>, as on the server
#   name    only the samples whose directory contains name, such as
#           heron3 or bigfour; the default is all of them
#
# It can be rerun at any time. Files that are already complete are
# skipped, interrupted downloads resume where they stopped, and a
# master that has changed on the server since it was downloaded (its
# ETag differs) is downloaded again. A download in progress is kept as
# <file>.part and renamed into place once it's complete; each file's
# ETag is kept beside it as .<file>.etag.
#
# Needs bash and curl. SAMPLES_BASE overrides where they come from.

set -euo pipefail

base=${SAMPLES_BASE:-https://scottstuff.net/webvideo}

# The masters, relative to $base. The server has no directory listing,
# so a new sample has to be added here.
files=(
	sample-20170121-birthday/20170121-birthday.mp4
	sample-20180529-band/20180529-band.mp4
	sample-20190421-cnc/20190421-cnc.mp4
	sample-20211022-naches/20211022-naches.mp4
	sample-20220716-jeep/20220716-jeep.mp4
	sample-20240708-hartspass/20240708-hartspass.mov
	sample-20240813-bighorn/20240813-bighorn.mov
	sample-20251206-heron3/20251206-heron3.mov
	sample-20260426-bigfour/20260426-bigfour-hdr.mov
	sample-20260426-bigfour/20260426-bigfour-sdr.mov
	sample-20260530-pyramid5/20260530-pyramid5-hdr.mov
	sample-20260530-pyramid5/20260530-pyramid5-sdr.mov
	sample-20260701-spada1/20260701-spada1-sdr.mov
	sample-20260919-yuri/20260919-yuri-sdr.mov
	sample-20261003-grist1/20261003-grist1-sdr.mov
	sample-20261025-snow2/20261025-snow2-8k-hdr.mov
	sample-20261025-snow2/20261025-snow2.mov
	sample-deception1/deception1-1080p-30s.mov
)

usage() {
	sed -n '/^#   \.\//,/default is all of them/p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

dest=samples
dry=false
while getopts nd:h opt; do
	case $opt in
	n) dry=true ;;
	d) dest=$OPTARG ;;
	*) usage ;;
	esac
done
shift $((OPTIND - 1))

# size prints a file's size in bytes, or 0 if it doesn't exist.
size() {
	if [ ! -e "$1" ]; then
		echo 0
	else
		stat -c %s "$1" 2>/dev/null || stat -f %z "$1"
	fi
}

gb() { awk -v b="$1" 'BEGIN { printf "%.2f GB", b / 1e9 }'; }

wanted() {
	[ $# -le 1 ] && return 0
	local f=$1 name
	shift
	for name in "$@"; do
		case ${f%%/*} in *"$name"*) return 0 ;; esac
	done
	return 1
}

matched=0 todo=0 todo_bytes=0 failed=0
for f in "${files[@]}"; do
	wanted "$f" ${@+"$@"} || continue
	matched=$((matched + 1))
	url=$base/$f
	out=$dest/$f
	part=$out.part
	etagfile=$(dirname "$out")/.$(basename "$out").etag

	# The last response's headers, in case of redirects.
	if ! headers=$(curl -sSfIL "$url" | tr -d '\r'); then
		echo "$f: can't reach $url" >&2
		failed=$((failed + 1))
		continue
	fi
	remote_size=$(awk 'tolower($1) == "content-length:" { n = $2 } END { print n + 0 }' <<<"$headers")
	etag=$(awk 'tolower($1) == "etag:" { $1 = ""; e = substr($0, 2) } END { print e }' <<<"$headers")

	if [ -f "$out" ] && [ "$(size "$out")" -eq "$remote_size" ]; then
		# A complete file without an ETag, such as one downloaded by
		# hand, is taken to be current.
		if [ ! -f "$etagfile" ]; then
			$dry || printf '%s\n' "$etag" >"$etagfile"
			continue
		fi
		[ "$(cat "$etagfile")" = "$etag" ] && continue
	fi

	# A partial download of an older version can't be resumed.
	have=$(size "$part")
	if [ -f "$part" ] && { [ ! -f "$part.etag" ] || [ "$(cat "$part.etag")" != "$etag" ]; }; then
		$dry || rm -f "$part" "$part.etag"
		have=0
	fi
	todo=$((todo + 1))
	todo_bytes=$((todo_bytes + remote_size - have))
	if $dry; then
		if [ "$have" -gt 0 ]; then
			echo "$f: $(gb $((remote_size - have))) of $(gb "$remote_size") left"
		else
			echo "$f: $(gb "$remote_size")"
		fi
		continue
	fi

	echo "$f: $(gb "$remote_size")"
	mkdir -p "$(dirname "$out")"
	printf '%s\n' "$etag" >"$part.etag"
	if [ "$have" -lt "$remote_size" ]; then
		if ! curl -fL --retry 5 --retry-delay 10 -C - --progress-bar -o "$part" "$url"; then
			echo "$f: download failed; rerun to resume" >&2
			failed=$((failed + 1))
			continue
		fi
	fi
	got=$(size "$part")
	if [ "$got" -ne "$remote_size" ]; then
		echo "$f: got $got bytes, expected $remote_size; rerun to resume" >&2
		failed=$((failed + 1))
		continue
	fi
	mv -f "$part" "$out"
	mv -f "$part.etag" "$etagfile"
done

if [ "$matched" -eq 0 ]; then
	echo "no samples match: $*" >&2
	exit 1
fi
if $dry; then
	echo "$todo of $matched files to download, $(gb "$todo_bytes")"
elif [ "$todo" -eq 0 ]; then
	echo "$dest is up to date ($matched files)"
fi
[ "$failed" -eq 0 ]
