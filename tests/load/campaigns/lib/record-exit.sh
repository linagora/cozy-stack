#!/usr/bin/env bash
# Runs a campaign step's producer and writes its own exit status to the file
# given first: make reports every failed recipe as 2.
#
# Usage, from run-campaign.sh: make <target> RECORD_EXIT="bash campaigns/lib/record-exit.sh <file>"
file=${1:?usage: record-exit.sh <file> <command> [args...]}
shift
"$@"
status=$?
echo "$status" > "$file"
exit "$status"
