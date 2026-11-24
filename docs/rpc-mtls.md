# Mutual TLS for RPC

With mutual TLS (mTLS) enabled, lnd requires a client certificate on its main
gRPC and REST listeners, including REST WebSocket upgrades. A client must
present a certificate signed by a trusted client certificate authority (CA),
and its leaf certificate must be on lnd's allowlist. This check applies before
the wallet is initialized or unlocked, when `WalletUnlocker` does not require
a macaroon. A certificate does not grant RPC permissions. Calls that normally
require a macaroon still require one.

## Register a client and enable mTLS

1. On the client machine, generate a private key and a certificate signing
   request (CSR). The command needs no connection to lnd and will not overwrite
   existing files. Choose a private output directory for the key, especially
   on Windows, where new files inherit the directory's access controls:

   ```sh
   lncli mtls new-client --keyout=client.key --csrout=client.csr
   ```

   Send only `client.csr` to your CA. Have it issue a certificate with the
   `clientAuth` extended key usage. Keep `client.key` on the client. If the
   CA uses intermediates, put the signed leaf first and then the intermediates
   in the client's PEM certificate file.
2. On the lnd host, save the trusted client CA certificates in a PEM file.
   This file contains public certificates, not the CA private key. Verify that
   the signed certificate belongs to the client you intend to enroll, then
   add its leaf certificate to the allowlist. Pass a PEM file containing only
   the leaf, not the full chain:

   ```sh
   lncli mtls allow-client --cert=client-leaf.pem \
     --allowlist=/etc/lnd/rpc-clients.allow
   ```

   This command does not connect to lnd. It records the SHA-256 fingerprint
   of the leaf (64 hex digits, without colons), and has no effect if the
   fingerprint is already present. The allowlist also accepts blank lines
   and comments starting with `#`. Run enrollment as the allowlist owner and
   use the configured path in a trusted directory. The command replaces the
   file by rename (atomic on Unix), preserving its permission bits but not
   its ownership or ACLs. Verify that lnd's account can read it. If a command
   is interrupted and leaves a `.lock` file, verify that no enrollment is
   running before removing the lock. Do not edit the allowlist while the
   command is running.
3. Set both paths in `lnd.conf`, or pass them to `lnd` as flags, then restart:

   ```ini
   rpcclientcapath=/etc/lnd/rpc-client-ca.pem
   rpcclientallowlistpath=/etc/lnd/rpc-clients.allow
   ```

With neither path set, lnd keeps its existing TLS behavior. If REST is enabled,
it must use TLS (`no-rest-tls` is incompatible with mTLS). Keep the allowlist
and CA file writable only by trusted operators. You don't need to change lnd's
server certificate; clients still verify it as before.

Without mTLS, the main RPC listener accepts WalletUnlocker calls only from a
loopback or Unix-domain connection. This applies to gRPC and REST, including
WebSocket requests. lnd checks the immediate transport peer and does not trust
forwarding headers. A local reverse proxy or other local process can therefore
make a remote client's request appear local; secure access to such proxies and
the local network namespace. Other RPC authorization remains unchanged.

To keep a remote unlock workflow without mTLS, set
`allowremotewalletunlocker=true` in `lnd.conf` or pass
`--allowremotewalletunlocker`. Only use this option if a trusted proxy or
private network controls access to RPC. The option does not disable client
certificate checks when mTLS is enabled.

## Connect with lncli or REST

For a one-off gRPC call, pass the client certificate and key together. Use
`--tlscertpath` for the **server** certificate when needed, and point
`--macaroonpath` at a macaroon with permission for the call:

```sh
lncli --rpcserver=lnd.example.net:10009 --tlscertpath=server.pem \
  --tlsclientcertpath=client.pem --tlsclientkeypath=client.key \
  --macaroonpath=readonly.macaroon getinfo
```

To load the same files automatically on later calls, save those settings to
an lncli profile:

```sh
lncli --rpcserver=lnd.example.net:10009 --tlscertpath=server.pem \
  --tlsclientcertpath=client.pem --tlsclientkeypath=client.key \
  --macaroonpath=readonly.macaroon profile add ops
lncli --profile=ops getinfo
```

The profile stores absolute paths, not the client key or certificate contents.
lncli reads the files at connection time. Pass **both** client path flags to
replace the saved identity for one invocation. lncli won't look for a client
identity at a default path or send one in `--insecure` mode.

For REST, present the same client credentials and send the macaroon as a header:

```sh
curl --cacert server.pem --cert client.pem --key client.key \
  -H "Grpc-Metadata-macaroon: $(xxd -p -c 256 readonly.macaroon | tr -d '\n')" \
  https://lnd.example.net:8080/v1/getinfo
```

## Rotate, revoke, or recover

To rotate, first add the replacement leaf fingerprint while leaving the old
one in place. Switch the client files, check that the new identity connects,
then remove the old fingerprint. To revoke a client, remove its fingerprint
and replace the allowlist atomically (write a temporary file in the same
directory, then rename it over the old file). lnd checks for changes once a
second. A missing or malformed replacement leaves the last valid list in
force. An empty, valid list denies all client certificates.

Once the removal takes effect, lnd rejects new requests from that certificate,
even over existing connections. It does not terminate RPC streams or WebSocket
sessions that it already admitted. If you're locked out, restore a valid
fingerprint through local access to the lnd host. Changing the client CA bundle
requires a restart.

## Compatibility

Use direct gRPC for macaroons with IP or IP-range caveats. With mTLS enabled,
the REST gateway connects to lnd in-process, so there is no IP peer for those
caveats to match. The dedicated remote signer listener is unchanged. The
legacy watch-only client doesn't present a certificate to the signer's main
gRPC listener, so don't enable mTLS on that listener while relying on legacy
remote signing.
