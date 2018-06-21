
  
# How to write a Rust gRPC client for the Lightning Network Daemon  
  
This section enumerates what you need to do to write a client that communicates with lnd in Rust.  
  
### Prerequisites  
 - [Rust](https://www.rust-lang.org/en-US/install.html)   
 - Google's Protocol Buffer compiler (`protoc`) installed and in `PATH`
  
### Setup and Installation  
Create a rust project:
```
cargo new project_name --bin
```
#### Project Structure  
```  
.
├── Cargo.toml
├── build.rs
└── src
    ├── lnd
    │   └── mod.rs
    ├── main.rs
    └── proto
        ├── google
        │   └── api
        │       ├── annotations.proto
        │       └── http.proto
        └── rpc.proto

5 directories, 7 files
```  
Note the ***proto*** folder, where all the proto files are kept.  
 - [annotations.proto](https://github.com/grpc-ecosystem/grpc-gateway/blob/master/third_party/googleapis/google/api/annotations.proto)  
 - [http.proto](https://github.com/grpc-ecosystem/grpc-gateway/blob/master/third_party/googleapis/google/api/http.proto)  
 - [rpc.proto](https://github.com/lightningnetwork/lnd/blob/master/lnrpc/rpc.proto)  
 

`build.rs` can be used to generate Rust modules from proto files by using [protoc-grpcio](https://crates.io/crates/protoc-grpcio).
```rust
extern crate protoc_grpcio;  
  
fn main() {
  let proto_file = "rpc.proto";
  let proto_root = "src/proto";  
  let rust_lnd = "src/lnd";    
  println!("cargo:rerun-if-changed={}", proto_root);  
  protoc_grpcio::compile_grpc_protos(
        &[proto_file],
        &[proto_root],
        &rust_lnd,  
  ).expect("Failed to compile gRPC definitions!");  
}
```  
The `Cargo.toml` might look like:
```rust
[package]  
name = "project_name"
version = "0.1.0"
build = "build.rs"

[lib]  
name = "lnd"
path = "src/lnd/mod.rs"

[dependencies]  
grpcio = "0.3.0"
protobuf = "2.0.2"
futures = "0.1.16"
  
[build-dependencies]  
protoc-grpcio = "0.2.0"
```  
To get access to the generated sub-module we have to override default private visibility in `mod.rs`:
```rust
extern crate protobuf;
extern crate grpcio;
extern crate futures;
  
pub mod rpc;
pub mod rpc_grpc;
```
 Running `cargo build` command should generate two files: `src/lnd/rpc.rs` and `src/lnd/rpc_grpc.rs`.
### Examples
We assume that `lnd` runs on the default `localhost:10009`. Depending on the OS ***tls.cert*** and ***admin.macaroon*** files can be found in `~/Library/Application Support/Lnd/` on macOS or in `~/.lnd/` on Linux.

#### GetInfo
```rust
extern crate grpcio;  
extern crate protobuf;  
extern crate futures;  
  
mod lnd;  
  
use std::env;  
use std::fs;  
use std::sync::Arc;  
use grpcio::ChannelCredentialsBuilder;  
use grpcio::ChannelBuilder;  
use grpcio::EnvBuilder;  
use lnd::rpc_grpc::LightningClient;  
  
use lnd::rpc::GetInfoRequest;  
  
fn main() {  
  let cert_path = env::home_dir().unwrap().join("Library/Application Support/Lnd/tls.cert");  
  let cert = fs::read(cert_path).expect("tls.cert file not found");  
  
  let env = Arc::new(EnvBuilder::new().build());  
  let credentials = ChannelCredentialsBuilder::new().root_cert(cert).build();  
  let channel = ChannelBuilder::new(env).secure_connect("localhost:10009", credentials);  
  let lnd_client = LightningClient::new(channel);  
  
  let response = lnd_client.get_info(&GetInfoRequest::new()).unwrap();  
  println!("response: {:#?}", response);  
  
  // access single field  
  println!("pubkey: {:#?}", response.block_height);  
}
```
#### AddInvoice
```rust
...
use lnd::rpc::Invoice;  
  
fn main() {
  ...
  let mut invoice_request = Invoice::new();
  // value of this invoice in satoshis  
  invoice_request.set_value(100);  
	  
  match lnd_client.add_invoice(&invoice_request) {  
    Ok(invoice) => println!("invoice: {:#?}", invoice),  
    Err(error) => println!("error: {:?}", error)
}
```

#### Using Macaroons
To authenticate using macaroons you need to include the macaroon in the metadata of the request.
```rust
...
use grpcio::CallOption;  
use grpcio::Metadata;  
use grpcio::MetadataBuilder;
use lnd::rpc::NetworkInfoRequest;
fn main() {
  ...
  let macaroon_path = env::home_dir().unwrap().join("Library/Application Support/Lnd/admin.macaroon");  
  let macaroon_binary = fs::read(macaroon_path).expect("admin.macaroon file not found");  
  // convert to hex String  
  let macaroon: String = macaroon_binary.iter().map(|b| format!("{:02X}", b)).collect();  
  
  let mut meta_builder = MetadataBuilder::with_capacity(1);  
  meta_builder.add_str("macaroon", &macaroon);  
  
  let call_option = CallOption::default().headers(meta_builder.build());
  // pass macaroon metadata with grpc call
  let response = lnd_client.get_network_info_opt(&NetworkInfoRequest::new(), call_option).unwrap();  
  
  println!("network info: {:#?}", response);
}
```