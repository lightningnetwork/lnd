#!/bin/bash

# Usage: ./gen_man_pages.sh DESTDIR PREFIX

# Check if lncli is installed.
function check_lncli_installed() {
    if [[ ! $(command -v lncli) ]]
    then
        echo "lncli could not be found. Please install lncli before running this script."
        exit 1
    fi
}

DESTDIR="$1"
PREFIX="$2"
FULLPATH="$DESTDIR$PREFIX"

check_lncli_installed

# Ignore warnings regarding HTMLBlock detection in go-md2man package
# since using "<...>" is part of our docs.
lncli generatemanpage 2>&1 | grep -v "go-md2man does not handle node type HTMLSpan" || true

mkdir -p "$FULLPATH/share/man/man1"

echo "Installing man pages to $FULLPATH/share/man/man1."
install -m 644 lnd.1 "$FULLPATH/share/man/man1/lnd.1"
install -m 644 lncli.1 "$FULLPATH/share/man/man1/lncli.1"

# Remove lncli.1 and lnd.1 artifacts from the current working directory.
rm -f lncli.1 lnd.1
