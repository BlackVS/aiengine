#!/bin/sh
# Fails when the module's Go source names a vendor credentials file or a
# vendor CLI's configuration directory. The CLI kind reads login state
# only from the CLI's own output (docs/TERMS-BOUNDARY.md); naming such a
# file in code is the first step towards reading it.
set -eu
cd "$(dirname "$0")/.."
pattern='\.credentials\.json|auth\.json|oauth_creds|application_default_credentials|credentials\.db|"\.(claude|codex|gemini)"|[/\]\.(claude|codex|gemini)([/\\"]|$)'
files=$(git ls-files -- '*.go')
if [ -z "$files" ]; then
	echo "no Go source found: run from a checkout"
	exit 1
fi
status=0
# The paths are the module's own, without spaces: word splitting is safe.
# shellcheck disable=SC2086
hits=$(grep -n -i -E "$pattern" $files) || status=$?
case $status in
0)
	echo "Go source names a vendor credentials file or configuration directory:"
	echo "$hits"
	exit 1
	;;
1) echo "ok: no vendor credentials file is named in the Go source" ;;
*)
	echo "grep failed (status $status)"
	exit 1
	;;
esac
