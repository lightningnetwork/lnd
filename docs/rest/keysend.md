# Sending a Keysend Payment in Python with `lnd`'s REST API endpoint

This document is born out of some personal frustration: I found it particularlly hard to find working examples of using the LND Rest API (specifically on an Umbrel) from Python.

I will present here some working code examples to send a Keysend payment from Python. At first you'd think this was trivial considering how easy it is with `lncli`:

`lncli sendpayment -d 0266ad2656c7a19a219d37e82b280046660f4d7f3ae0c00b64a1629de4ea567668 -a 1948 --keysend --data 818818=627269616e6f666c6f6e646f6e --json`

That will send 1948 sats to the public key `0266ad2656c7a19a219d37e82b280046660f4d7f3ae0c00b64a1629de4ea567668` and add a special key of `818818` which passes the Hive address of `brianoflondon` as Hex: `627269616e6f666c6f6e646f6e`

To find this Hex value in Python:
```
>>> a.encode().hex()
'627269616e6f666c6f6e646f6e'
```

## How to do that with Python:

I'm going to assume you've successfully managed to get a connection up and running to your LND API. If you haven't perhaps that needs to be better explained somewhere in these docs. Perhaps I can be persuaded.

I've documented this code and whilst its a bit different from what I'm actually using (I'm working on streaming value 4 value payments in podcasting so I'm sending quite a bit more information encoded in the `dest_custom_records` field but the method is exactly the same as I've shown here for the `818818` field). [You can learn more about these Podcasting specific fields here](https://github.com/satoshisstream/satoshis.stream/blob/main/TLV_registry.md).

```python

import base64
import codecs
import json
import os
from hashlib import sha256
from secrets import token_hex
from typing import Tuple

import httpx



def get_lnd_headers_cert(
    admin: bool = False, local: bool = False, node: str = None
) -> Tuple[dict, str]:
    """Return the headers and certificate for connecting, if macaroon passed as string
    does not return a certificate (Voltage)"""
    if not node:
        node = Config.LOCAL_LND_NODE_ADDRESS

    # maintain option to work with local macaroon and umbrel
    macaroon_folder = ".macaroon"
    if not admin:
        macaroon_file = "invoices.macaroon"
    else:
        macaroon_file = "admin.macaroon"
    macaroon_path = os.path.join(macaroon_folder, macaroon_file)
    cert = os.path.join(macaroon_folder, "tls.cert")
    macaroon = codecs.encode(open(macaroon_path, "rb").read(), "hex")
    headers = {"Grpc-Metadata-macaroon": macaroon}
    return headers, cert


def b64_hex_transform(plain_str: str) -> str:
    """Returns the b64 transformed version of a hex string"""
    a_string = bytes.fromhex(plain_str)
    return base64.b64encode(a_string).decode()


def b64_transform(plain_str: str) -> str:
    """Returns the b64 transformed version of a string"""
    return base64.b64encode(plain_str.encode()).decode()


def send_keysend(
    amt: int,
    dest_pubkey: str = "",
    hive_accname: str = "brianoflondon",
) -> dict:
    """Pay a keysend invoice using the chosen node"""
    node = "https://umbrel.local:8080/"
    headers, cert = get_lnd_headers_cert(admin=True, node=node)
    if not dest_pubkey:
        dest_pubkey = my_voltage_public_key

    # Base 64 encoded destination bytes
    dest = b64_hex_transform(dest_pubkey)

    # We generate a random 32 byte Hex pre_image here.
    pre_image = token_hex(32)
    # This is the hash of the pre-image
    payment_hash = sha256(bytes.fromhex(pre_image))

    # The record 5482373484 is special: it carries the pre_image
    # to the destination so it can be compared with the hash we
    # pass via the payment_hash
    dest_custom_records = {
        5482373484: b64_hex_transform(pre_image),
        818818: b64_transform(hive_accname),
    }

    url = f"{node}v1/channels/transactions"
    data = {
        "dest": dest,
        "amt": amt,
        "payment_hash": b64_hex_transform(payment_hash.hexdigest()),
        "dest_custom_records": dest_custom_records,
    }

    response = httpx.post(
        url=url, headers=headers, data=json.dumps(data), verify=cert
        )

    print(json.dumps(response.json(), indent=2))
    return json.dumps(response.json(), indent=2)

```


This explanation of what is going on here is pretty useful too:

>Since you're not paying an invoice, the receiver doesn't know the preimage for the given payment's hash. You need to add the preimage to your custom records. The number is 5482373484.
>
>It also looks like you need to actually set the payment's hash (payment_hash=sha256(preimage)) of the payment.
>
>See https://github.com/lightningnetwork/lnd/blob/master/cmd/lncli/cmd_payments.go#L358 on how it's done on the RPC level (what's behind the --keysend flag of lndcli sendpayment).

If you still have question, find me @brianoflondon pretty much anywhere online and I'll help if I can.