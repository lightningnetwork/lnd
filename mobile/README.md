## Building mobile libraries

### Prerequisites
#### protoc
Install the dependencies for genarating protobuf definitions as stated in [lnrpc docs](
../lnrpc/README.md#generate-protobuf-definitions)

#### gomobile
Follow [gomobile](https://github.com/golang/go/wiki/Mobile) in order to intall `gomobile` and dependencies.

Remember to run `gomobile init` (otherwise the `lnd` build might just hang).

Note that `gomobile` only supports building projects from `GOPATH` at this point.

#### falafel
Install [`falafel`](https://github.com/halseth/falafel):
```
go get -u -v github.com/halseth/falafel
```

### Building `lnd` for iOS
```
make ios
```

### Building `lnd` for Android
```
make android
```

`make mobile` will build both iOS and Android libs.

### Libraries
After the build has succeeded, the libraries will be found in `mobile/build/ios/Lndmobile.framework` and `mobile/build/android/Lndmobile.aar`. Reference your platforms' SDK documentation for how to add the library to your project.

#### Generating proto definitions for your language.
In order to call the methods in the generated library, the serialized proto for the given RPC call must be provided. Similarly, the response will be a serialized proto.

In order to generate protobuf definitions for your language of choice, add the proto plugin to the `protoc` invocations found in [`gen_protos.sh`](../lnrpc/gen_protos.sh). For instance to generate protos for Swift, add `--swift_out=.` and run `make rpc`.

### Options
Similar to lnd, subservers can be conditionally compiled with the build by setting the tags argument:

```
make ios tags="routerrpc"
```

To support subservers that have APIs with name conflicts, pass the "prefix" flag. This will add the subserver name as a prefix to each method name:

```
make ios tags="routerrpc" prefix=1
```

### API docs
TODO(halseth)
