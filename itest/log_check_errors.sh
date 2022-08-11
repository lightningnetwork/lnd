#!/bin/bash

BASEDIR=$(dirname "$0")

echo ""

# Filter all log files for errors, substitute variable data and match against whitelist.
find $BASEDIR -name "*.log" | xargs grep -h "\[ERR\]" | \
sed -r -f $BASEDIR/log_substitutions.txt | \
sort | uniq | \
grep -Fvi -f $BASEDIR/log_error_whitelist.txt

# If something shows up (not on whitelist) exit with error code 1.
if [[ $? -eq 0 ]]; then
        echo ""
        echo "In the itest logs, the log line (patterns) above were detected."
        echo "[ERR] lines are generally reserved for internal errors."
        echo "Resolve the issue by either changing the log level or adding an "
        echo "exception to log_error_whitelist.txt"
        echo ""

        exit 1
fi

echo "No itest errors detected."
echo ""
