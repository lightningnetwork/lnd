#!/bin/bash

BASEDIR=$(dirname "$0")

# Filter all log files for errors, substitute variable data and match against whitelist.
cat $BASEDIR/*.log | grep "\[ERR\]" | \
sed -r -f $BASEDIR/log_substitutions.txt | \
sort | uniq | \
grep -Fvi -f $BASEDIR/log_error_whitelist.txt

# If something shows up (not on whitelist) exit with error code 1.
test $? -eq 1
