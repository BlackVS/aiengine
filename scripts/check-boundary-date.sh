#!/bin/sh
# Fails when docs/TERMS-BOUNDARY.md was last checked more than 180 days
# ago. The first line must read "Checked: YYYY-MM-DD".
set -eu
cd "$(dirname "$0")/.."
max_days=180
line=$(head -n 1 docs/TERMS-BOUNDARY.md | tr -d '\r')
checked=${line#Checked: }
case "$checked" in
[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]) ;;
*)
	echo "docs/TERMS-BOUNDARY.md must start with 'Checked: YYYY-MM-DD', found: $line"
	exit 1
	;;
esac
then_s=$(date -u -d "$checked" +%s)
now_s=$(date -u +%s)
age=$(((now_s - then_s) / 86400))
if [ "$age" -gt "$max_days" ]; then
	echo "docs/TERMS-BOUNDARY.md was checked $checked, $age days ago (limit $max_days): re-check the vendor pages (see its Re-check procedure)"
	exit 1
fi
if [ "$age" -lt 0 ]; then
	echo "docs/TERMS-BOUNDARY.md has a date in the future: $checked"
	exit 1
fi
echo "ok: terms boundary checked $checked, $age days ago"
