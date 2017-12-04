# How to write a Python gRPC client for the Lightning Network Daemon

This section enumerates what you need to do to write a client that communicates
with lnd in Python.

### Setup and Installation

Lnd uses the gRPC protocol for communication with clients like lncli. gRPC is
based on protocol buffers and as such, you will need to compile the lnd proto
file in Python before you can use it to communicate with lnd.

* Create a virtual environment for your project
```
$ virtualenv lnd
```
* Activate the virtual environment
```
$ source lnd/bin/activate
```
* Install dependencies (googleapis-common-protos is required due to the use of
  google/api/annotations.proto)
```
(lnd)$ pip install grpcio grpcio-tools googleapis-common-protos
```
* Clone the google api's repository (required due to the use of
  google/api/annotations.proto)
```
(lnd)$ git clone https://github.com/googleapis/googleapis.git
```
* Copy the lnd rpc.proto file (you'll find this at
  [lnrpc/rpc.proto](https://github.com/lightningnetwork/lnd/blob/master/lnrpc/rpc.proto))
  or just download it
```
(lnd)$ curl -o rpc.proto -s https://raw.githubusercontent.com/lightningnetwork/lnd/master/lnrpc/rpc.proto
```
* Compile the proto file
```
(lnd)$ python -m grpc_tools.protoc --proto_path=googleapis:. --python_out=. --grpc_python_out=. rpc.proto
```

After following these steps, two files `rpc_pb2.py` and `rpc_pb2_grpc.py` will
be generated. These files will be imported in your project anytime you use
Python gRPC.

#### Imports and Client

Everytime you use Python gRPC, you will have to import the generated rpc modules
and set up a channel and stub to your connect to your `lnd` node:

```python
import rpc_pb2 as ln
import rpc_pb2_grpc as lnrpc
import grpc

# Lnd cert is at ~/.lnd/tls.cert on Linux and
# ~/Library/Application Support/Lnd/tls.cert on Mac
cert = open('~/.lnd/tls.cert').read()
creds = grpc.ssl_channel_credentials(cert)
channel = grpc.secure_channel('localhost:10009', creds)
stub = lnrpc.LightningStub(channel)
```

### Examples

Let's walk through some examples of Python gRPC clients. These examples assume
that you have at least two `lnd` nodes running, the RPC location of one of which
is at the default `localhost:10009`, with an open channel between the two nodes.

#### Simple RPC

```python
# Retrieve and display the wallet balance
response = stub.WalletBalance(ln.WalletBalanceRequest(witness_only=True))
print response.total_balance
```

#### Response-streaming RPC

```python
request = ln.InvoiceSubscription()
for invoice in stub.SubscribeInvoices(request);
    print invoice
```

Now, create an invoice for your node at `localhost:10009`and send a payment to
it from another node.
```bash
$ lncli addinvoice --value=100
{
	"r_hash": <R_HASH>,
	"pay_req": <PAY_REQ>
}
$ lncli sendpayment --pay_req=<PAY_REQ>
```

Your Python console should now display the details of the recently satisfied
invoice.

#### Bidirectional-streaming RPC

```python
from time import sleep
import codecs

def request_generator(dest, amt):
      # Initialization code here
      counter = 0
      print("Starting up")
      while True:
          request = ln.SendRequest(
              dest=dest,
              amt=amt,
          )
          yield request
          # Alter parameters here
          counter += 1
          sleep(2)

# Outputs from lncli are hex-encoded
dest_hex = <RECEIVER_ID_PUBKEY>
dest_bytes = codecs.decode(dest_hex, 'hex')

request_iterable = request_generator(dest=dest_bytes, amt=100)

for payment in stub.SendPayment(request_iterable):
    print payment
```
This example will send a payment of 100 satoshis every 2 seconds.

### Conclusion

With the above, you should have all the `lnd` related `gRPC` dependencies
installed locally into your virtual environment. In order to get up to speed
with `protofbuf` usage from Python, see [this official `protobuf` tutorial for
Python](https://developers.google.com/protocol-buffers/docs/pythontutorial).
Additionally, [this official gRPC
resource](http://www.grpc.io/docs/tutorials/basic/python.html) provides more
details around how to drive `gRPC` from Python.
