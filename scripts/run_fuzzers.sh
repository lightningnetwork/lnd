#!/bin/bash

GO111MODULE=on

# Build go-fuzz & go-fuzz-build.
go get -u github.com/dvyukov/go-fuzz/go-fuzz github.com/dvyukov/go-fuzz/go-fuzz-build
# go get -u github.com/dvyukov/go-fuzz/...

go get -d -v github.com/lightningnetwork/lnd

# Get the seeds from the repo.
#git clone https://github.com/Crypt-iQ/lnd_fuzz_seeds

#GO111MODULE=on

ls $GOPATH
ls $GOPATH/src
ls $GOPATH/src/github.com/lightningnetwork/lnd
echo "--gopath src"

ls $LND_DIR
echo "lsing"
echo "starting dir $(pwd)"
# For every folder in the fuzz directory, build and run the fuzzers.
cd $LND_DIR
for folder in $(ls fuzz):
do
	echo "now $(pwd)"
	echo "now folder $folder"
	echo "ls dir $(ls)"
	cd
	cd $LND_DIR/fuzz/$folder
	echo "Changed to $(pwd)"
	echo "Building $folder fuzzers"
	find * -maxdepth 1 -regex '[A-Za-z0-9\-_.]'* -not -name fuzz_utils.go | sed 's/\.go$//1' | xargs -I % sh -c '$GOPATH/bin/go-fuzz-build -func Fuzz_% -o $folder-%-fuzz.zip $LND_DIR/fuzz/$folder'

	echo "Done building $folder fuzzers"

	# Run the fuzzers with the seeds.
done

echo "Done building fuzzing binaries"
