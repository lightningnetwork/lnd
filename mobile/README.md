# Building mobile libraries

## Prerequisites

### Docker

- Install and run [Docker](https://www.docker.com/products/docker-desktop).

### Make

- Check that `make` is available by running the following command without errors:

```shell
⛰  make --version
```

### Go mobile

Install [gomobile](https://github.com/golang/go/wiki/Mobile) and initialize it:

```shell
⛰  go install golang.org/x/mobile/cmd/gomobile@latest
⛰  gomobile init
```

## Building the libraries

Note that `gomobile` only supports building projects from `GOPATH` at this
point. So, before continuing, be sure to be in the `src` folder:

```shell
⛰  cd $GOPATH/src/github.com/lightningnetwork/lnd
```

To checkout the latest tagged release of lnd, run

```shell
⛰  git checkout $(git describe --tags --abbrev=0)
```

### Building `lnd` for iOS

```shell
⛰  make ios
```

### Building `lnd` for Android

```shell
⛰  make android
```

`make mobile` will build both iOS and Android libraries.

### Libraries

After the build has succeeded, the libraries will be found in
`mobile/build/ios/Lndmobile.xcframework` and
`mobile/build/android/Lndmobile.aar`. Reference your platforms' SDK
documentation for how to add the library to your project.

## Generating proto definitions

In order to call the methods in the generated library, the serialized proto for
the given RPC call must be provided. Similarly, the response will be a
serialized proto.

### iOS

In order to generate protobuf definitions for iOS, add `--swift_out=.` to the
first `protoc` invocation found in [ `gen_protos.sh` ](../lnrpc/gen_protos.sh).

Then, some changes to [Dockerfile](../lnrpc/Dockerfile) need to be done in
order to use the [Swift protobuf](https://github.com/apple/swift-protobuf)
plugin with protoc:

1. Replace the base image with `FROM swift:focal` so that Swift can be used.
2. `clang-format='1:7.0*'` is unavailable in Ubuntu Focal. Change that to
`clang-format='1:10.0*`.
3. On the next line, install Go and set the environment variables by adding the
following commands:

```
RUN apt-get install -y wget \
    && wget -c https://golang.org/dl/go1.17.3.linux-amd64.tar.gz -O - \
    | tar -xz -C /usr/local
ENV GOPATH=/go
ENV PATH=$PATH:/usr/local/go/bin:/go/bin
```

4. At the end of the file, just above `CMD`, add the following `RUN` command.
This will download and compile the latest tagged release of Swift protobuf.

```
RUN git clone https://github.com/apple/swift-protobuf.git \
&& cd swift-protobuf \ 
&& git checkout $(git describe --tags --abbrev=0) \
&& swift build -c release \
&& mv .build/release/protoc-gen-swift /bin
```

Finally, run `make rpc`.

Tip: The generated Swift files will be found in various folders. If you’d like
to move them to the same folder as the framework file, run

```shell
⛰  `find . -name "*.swift" -print0 | xargs -0 -I {} mv {} mobile/build/ios`.
```

`Lndmobile.xcframework` and all Swift files should now be added to your Xcode
project. You will also need to add [Swift Protobuf](https://github.com/apple/swift-protobuf)
to your project to support the generated code.  

### Android

In order to generate protobuf definitions for Android, add `--java_out=.`

to the first `protoc` invocation found in
[ `gen_protos.sh` ](../lnrpc/gen_protos.sh). Then, run `make rpc`.

## Options

Similar to lnd, subservers can be conditionally compiled with the build by
setting the tags argument:

```shell
⛰  make ios
```

To support subservers that have APIs with name conflicts, pass the "prefix"
flag. This will add the subserver name as a prefix to each method name:

```shell
⛰  make ios prefix=1
```

## API docs

[LND gRPC API Reference](https://api.lightning.community)

TODO(halseth)
