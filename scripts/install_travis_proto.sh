#!/usr/bin/env bash

set -ev

PROTOC_VERSION=3.4.0
PROTOBUF_VERSION="aa810b61a9c79d51363740d207bb46cf8e620ed5"
GENPROTO_VERSION="a8101f21cf983e773d0c1133ebc5424792003214"
GRPC_GATEWAY_VERSION="f2862b476edcef83412c7af8687c9cd8e4097c0f"

install_protoc () {
        rm -rf ~/protocdl/protoc
        if [ -d ~/protocdl/protoc ]
        then
                echo "using cached protoc"
        else
                mkdir -p ~/protocdl
                pushd ~/protocdl
                wget https://github.com/google/protobuf/releases/download/v$PROTOC_VERSION/protoc-$PROTOC_VERSION-linux-x86_64.zip
                unzip -o protoc-$PROTOC_VERSION-linux-x86_64.zip -d ./protoc
                chmod +x ./protoc/bin/protoc
                export PATH=~/protocdl/protoc/bin:$PATH
                sudo cp -r ./protoc/bin ./protoc/include /usr/local
                sudo chmod +r /usr/local/include -R
                popd
        fi
}



install_protobuf () {
        if [ -d $GOPATH/src/github.com/golang/protobuf ]
        then
                rm -rf $GOPATH/src/github.com/golang/protobuf
        fi
        git clone https://github.com/golang/protobuf $GOPATH/src/github.com/golang/protobuf
        pushd $GOPATH/src/github.com/golang/protobuf
        git reset --hard $PROTOBUF_VERSION
        make
        popd
}
        

install_genproto () {
        if [ ! -d $GOPATH/src/google.golang.org/genproto ]
        then
                git clone https://github.com/google/go-genproto $GOPATH/src/google.golang.org/genproto
        fi
        pushd $GOPATH/src/google.golang.org/genproto
        git reset --hard $GENPROTO_VERSION
        popd
}

install_grpc_ecosystem () {
        if [ ! -d $GOPATH/src/github.com/grpc-ecosystem/grpc-gateway ]
        then
                git clone https://github.com/grpc-ecosystem/grpc-gateway $GOPATH/src/github.com/grpc-ecosystem/grpc-gateway
        fi
        pushd $GOPATH/src/github.com/grpc-ecosystem/grpc-gateway
        git reset --hard $GRPC_GATEWAY_VERSION
        GO111MODULE=on go install ./protoc-gen-grpc-gateway ./protoc-gen-swagger
        popd
}

install_protoc
install_protobuf
install_genproto
install_grpc_ecosystem
