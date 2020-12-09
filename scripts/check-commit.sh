#!/bin/bash
set -e
set -x
echo Testing $(git log -1 --oneline)
make unit pkg=... case=_NONE_
