#!/bin/bash

dest="$1"
if [[ -z "$dest" ]]; then
    dest="data.go"
fi

# apparently BSD base64 uses -b and GNU uses -w. Hmm.
base64flags="-w"
base64 -w &> /dev/null
if [[ "$?" -ne "0" ]]; then
    base64flags="-b"
fi

echo -e "package main\n\n// this file is auto-generated by bundle.sh\n" > $dest

bundle() {
    for f in $1/*; do
        echo -ne "\t\"$f\": \`" >> $dest
        cat $f |base64 $base64flags 100 >> $dest
        echo -ne "\`,\n" >> $dest
    done
}

echo "var _bundle = map[string]string{" >> $dest

bundle templates
bundle static

echo -e "}\n" >> $dest

