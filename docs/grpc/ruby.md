# How to write a Ruby gRPC client for the Lightning Network Daemon

This section enumerates what you need to do to write a client that communicates
with `lnd` in Ruby.

### Introduction

`lnd` uses the `gRPC` protocol for communication with clients like `lncli`.

`gRPC` is based on protocol buffers and as such, you will need to compile
the `lnd` proto file in Ruby before you can use it to communicate with `lnd`.

### Setup

Install gRPC rubygems:

```
$ gem install grpc
$ gem install grpc-tools
```

Clone the Google APIs repository:

```
$ git clone https://github.com/googleapis/googleapis.git
```

Fetch the `rpc.proto` file (or copy it from your local source directory):

```
$ curl -o rpc.proto -s https://raw.githubusercontent.com/lightningnetwork/lnd/master/lnrpc/rpc.proto
```

Compile the proto file:

```
$ grpc_tools_ruby_protoc --proto_path googleapis:. --ruby_out=. --grpc_out=. rpc.proto
```

Two files will be generated in the current directory: 

* `rpc_pb.rb`
* `rpc_services_pb.rb`

### Examples

#### Simple client to display wallet balance

Every time you use the Ruby gRPC you need to require the `rpc_services_pb` file.

We assume that `lnd` runs on the default `localhost:10009`.

We further assume you run `lnd` with `--no-macaroons`.

```ruby
#!/usr/bin/env ruby

$:.unshift(File.dirname(__FILE__))

require 'grpc'
require 'rpc_services_pb'

# Due to updated ECDSA generated tls.cert we need to let gprc know that
# we need to use that cipher suite otherwise there will be a handhsake
# error when we communicate with the lnd rpc server.
ENV['GRPC_SSL_CIPHER_SUITES'] = "HIGH+ECDSA"

certificate = File.read(File.expand_path("~/.lnd/tls.cert"))
credentials = GRPC::Core::ChannelCredentials.new(certificate)
stub = Lnrpc::Lightning::Stub.new('127.0.0.1:10009', credentials)

response = stub.wallet_balance(Lnrpc::WalletBalanceRequest.new())
puts "Total balance: #{response.total_balance}"
```

This will show the `total_balance` of the wallet.

#### Streaming client for invoice payment updates

```ruby
#!/usr/bin/env ruby

$:.unshift(File.dirname(__FILE__))

require 'grpc'
require 'rpc_services_pb'

ENV['GRPC_SSL_CIPHER_SUITES'] = "HIGH+ECDSA"

certificate = File.read(File.expand_path("~/.lnd/tls.cert"))
credentials = GRPC::Core::ChannelCredentials.new(certificate)
stub = Lnrpc::Lightning::Stub.new('127.0.0.1:10009', credentials)

stub.subscribe_invoices(Lnrpc::InvoiceSubscription.new) do |invoice|
  puts invoice.inspect
end
```

Now, create an invoice on your node:

```bash
$ lncli addinvoice --amt=590
{
	"r_hash": <R_HASH>,
	"pay_req": <PAY_REQ>
}
```

Next send a payment to it from another node:

```
$ lncli sendpayment --pay_req=<PAY_REQ>
```

You should now see the details of the settled invoice appear.

#### Using Macaroons

To authenticate using macaroons you need to include the macaroon in the metadata of the request.

```ruby
# Lnd admin macaroon is at ~/.lnd/admin.macaroon on Linux and
# ~/Library/Application Support/Lnd/admin.macaroon on Mac
macaroon_binary = File.read(File.expand_path("~/.lnd/admin.macaroon"))
macaroon = macaroon_binary.each_byte.map { |b| b.to_s(16).rjust(2,'0') }.join
```

The simplest approach to use the macaroon is to include the metadata in each request as shown below.

```ruby
stub.get_info(Lnrpc::GetInfoRequest.new, metadata: {metadata: macaroon})
```

However, this can get tiresome to do for each request. We can use gRPC interceptors to add this metadata to each request automatically. Our interceptor class would look like this.

```ruby
class MacaroonInterceptor < GRPC::ClientInterceptor
  attr_reader :macaroon

  def initialize(macaroon)
    @macaroon = macaroon
    super
  end

  def request_response(request:, call:, method:, metadata:)
    metadata['macaroon'] = macaroon
    yield
  end
end
```

And then we would include it when we create our stub like so.

```ruby
certificate = File.read(File.expand_path("~/.lnd/tls.cert"))
credentials = GRPC::Core::ChannelCredentials.new(certificate)
macaroon_binary = File.read(File.expand_path("~/.lnd/admin.macaroon"))
macaroon = macaroon_binary.each_byte.map { |b| b.to_s(16).rjust(2,'0') }.join

stub = Lnrpc::Lightning::Stub.new(
	'localhost:10009',
	credentials,
	interceptors: [MacaroonInterceptor.new(macaroon)]
)

# Now we don't need to pass the metadata on a request level
p stub.get_info(Lnrpc::GetInfoRequest.new)
```
