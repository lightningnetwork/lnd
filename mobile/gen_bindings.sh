#!/bin/sh

mkdir -p build

# Check falafel version.
falafelVersion="0.5"
falafel=$(which falafel)
if [ $falafel ]
then
        version=$($falafel -v)
        if [ $version != $falafelVersion ]
        then
                echo "falafel version $falafelVersion required"
                exit 1
        fi
        echo "Using plugin $falafel $version"
else
        echo "falafel not found"
        exit 1
fi

pkg="lndmobile"
target_pkg="github.com/lightningnetwork/lnd/lnrpc"

# Generate APIs by passing the parsed protos to the falafel plugin.
opts="package_name=$pkg,target_package=$target_pkg,listeners=lightning=lightningLis walletunlocker=walletUnlockerLis,mem_rpc=1"
protoc -I/usr/local/include -I. \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --plugin=protoc-gen-custom=$falafel\
       --custom_out=./build \
       --custom_opt="$opts" \
       --proto_path=../lnrpc \
       rpc.proto

# If prefix=1 is specified, prefix the generated methods with subserver name.
# This must be enabled to support subservers with name conflicts.
use_prefix="0"
if [[ $prefix = "1" ]]
then
    echo "Prefixing methods with subserver name"
    use_prefix="1"
fi

# Find all subservers.
for file in ../lnrpc/**/*.proto
do
    DIRECTORY=$(dirname ${file})
    tag=$(basename ${DIRECTORY})
    build_tags="// +build $tag"
    lis="lightningLis"

    opts="package_name=$pkg,target_package=$target_pkg/$tag,build_tags=$build_tags,api_prefix=$use_prefix,defaultlistener=$lis"

    echo "Generating mobile protos from ${file}, with build tag ${tag}"

    protoc -I/usr/local/include -I. \
           -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
           -I../lnrpc \
           --plugin=protoc-gen-custom=$falafel \
           --custom_out=./build \
           --custom_opt="$opts" \
           --proto_path=${DIRECTORY} \
           ${file}
done
