# lnd wallet initializer

This directory contains the source for the optional to use `lndinit` command.
The main purpose of `lndinit` is to help automate the `lnd` wallet
initialization, including seed and password generation.

## Example use case 1: Basic setup

This is a very basic example that shows the purpose of the different sub
commands of the `lndinit` binary. In this example, all secrets are stored in
files. This is normally not a good security practice as potentially other users
or processes on a system can read those secrets if the permissions aren't set
correctly. It is advised to store secrets in dedicated secret storage services
like Kubernetes Secrets or HashiCorp Vault.

### 1. Generate a seed without a seed passphrase

Create a new seed if one does not exist yet.

```shell
$ if [[ ! -f /safe/location/seed.txt ]]; then
    lndinit gen-seed > /safe/location/seed.txt
  fi
```

### 2. Generate a wallet password

Create a new wallet password if one does not exist yet.

```shell
$ if [[ ! -f /safe/location/walletpassword.txt ]]; then
    lndinit gen-password > /safe/location/walletpassword.txt
  fi
```

### 3. Initialize the wallet

Create the wallet database with the given seed and password files. If the wallet
already exists, we make sure we can actually unlock it with the given password
file. This will take a few seconds in any case.

```shell
$ lndinit -v init-wallet \
    --secret-source=file \
    --file.seed=/safe/location/seed.txt \
    --file.wallet-password=/safe/location/walletpassword.txt \
    --output-wallet-dir=$HOME/.lnd/data/chain/bitcoin/mainnet \
    --validate-password
```

### 4. Start and auto unlock lnd

With everything prepared, we can now start lnd and instruct it to auto unlock
itself with the password in the file we prepared.

```shell
$ lnd \
    --bitcoin.active \
    ...
    --wallet-unlock-password-file=/safe/location/walletpassword.txt
```

## Example use case 2: Kubernetes

This example shows how Kubernetes (k8s) Secrets can be used to store the wallet
seed and password. The pod running those commands must be provisioned with a
service account that has permissions to read/create/modify secrets in a given
namespace.

Here's an example of a service account, role provision and pod definition:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: lnd-provision-account


---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: lnd-update-secrets-role
  namespace: default
rules:
  - apiGroups: [ "" ]
    resources: [ "secrets" ]
    verbs: [ "get", "list", "watch", "update" ]


---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: lnd-update-secrets-role-binding
  namespace: default
roleRef:
  kind: Role
  name: lnd-update-secrets-role
  apiGroup: rbac.authorization.k8s.io
subjects:
  - kind: ServiceAccount
    name: lnd-provision-account
    namespace: default


---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lnd-pod
spec:
  strategy:
    type: Recreate
  replicas: 1
  template:
    spec:
      # We use the special service account created, so the init script is able
      # to update the secret as expected.
      serviceAccountName: lnd-provision-account

      containers:
        # The main lnd container
        - name: lnd
          image: lightninglabs/lnd:v0.14.2-beta
          env:
            - name: WALLET_SECRET_NAME
              value: lnd-wallet-secret
            - name: WALLET_DIR
              value: /root/.lnd/data/chain/bitcoin/mainnet
            - name: CERT_DIR
              value: /root/.lnd
            - name: UPLOAD_RPC_SECRETS
              value: '1'
            - name: RPC_SECRETS_NAME
              value: lnd-rpc-secrets
          command: [ '/init-wallet-k8s.sh' ]
          args: [
              '--bitcoin.mainnet',
              '...',
              '--wallet-unlock-password-file=/tmp/wallet-password',
          ]
```

The `/init-wallet-k8s.sh` script that is invoked in the example above can be
found at the end of this document.
The script executes the steps described in this example and also uploads the
RPC secrets (`tls.cert` and all `*.macaroon` files) to another secret so apps
using the `lnd` node can access those secrets.

### 1. Generate a seed passphrase (optional)

Generate a new seed passphrase. If an entry already exists in the k8s secret, it
is not overwritten, and the operation is a no-op.

```shell
$ lndinit gen-password \
    | lndinit -v store-secret \
    --target=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.secret-entry-name=seed-passphrase
```

### 2. Generate a seed using the passphrase

Generate a new seed with the passphrase created before. If an entry already
exists in the k8s secret, it is not overwritten, and the operation is a no-op.

```shell
$ lndinit -v gen-seed \
    --passphrase-k8s.secret-name=lnd-secrets \
    --passphrase-k8s.secret-entry-name=seed-passphrase \
    | lndinit -v store-secret \
    --target=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.secret-entry-name=seed
```

### 3. Generate a wallet password

Generate a new wallet password. If an entry already exists in the k8s secret, it
is not overwritten, and the operation is a no-op.

```shell
$ lndinit gen-password \
    | lndinit -v store-secret \
    --target=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.secret-entry-name=wallet-password
```

### 4. Initialize the wallet, attempting a test unlock with the password

Create the wallet database with the given seed, seed passphrase and wallet
password loaded from a k8s secret. If the wallet already exists, we make sure we
can actually unlock it with the given password file. This will take a few
seconds in any case.

```shell
$ lndinit -v init-wallet \
    --secret-source=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.seed-entry-name=seed \
    --k8s.seed-passphrase-entry-name=seed-passphrase \
    --k8s.wallet-password-entry-name=wallet-password \
    --init-file.output-wallet-dir=$HOME/.lnd/data/chain/bitcoin/mainnet \
    --init-file.validate-password
```

The above is an example for a file/bbolt based node. For such a node creating
the wallet directly as a file is the most secure option, since it doesn't
require the node to spin up the wallet unlocker RPC (which doesn't use macaroons
and is therefore un-authenticated).

But in setups where the wallet isn't a file (since all state is in a remote
database such as etcd or Postgres), this method cannot be used.
Instead, the wallet needs to be initialized through RPC, as shown in the next
example:

```shell
$ lndinit -v init-wallet \
    --secret-source=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.seed-entry-name=seed \
    --k8s.seed-passphrase-entry-name=seed-passphrase \
    --k8s.wallet-password-entry-name=wallet-password \
    --init-type=rpc \
    --init-rpc.server=localhost:10009 \
    --init-rpc.tls-cert-path=$HOME/.lnd/tls.cert
```

**NOTE**: If this is used in combination with the
`--wallet-unlock-password-file=` flag in `lnd` for automatic unlocking, then the
`--wallet-unlock-allow-create` flag also needs to be set. Otherwise, `lnd` won't
be starting the wallet unlocking RPC that is used for initializing the wallet.

The following example shows how to use the `lndinit init-wallet` command to
create a watch-only wallet from a previously exported accounts JSON file:

```shell
$ lndinit -v init-wallet \
    --secret-source=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.seed-entry-name=seed \
    --k8s.seed-passphrase-entry-name=seed-passphrase \
    --k8s.wallet-password-entry-name=wallet-password \
    --init-type=rpc \
    --init-rpc.server=localhost:10009 \
    --init-rpc.tls-cert-path=$HOME/.lnd/tls.cert \
    --init-rpc.watch-only \
    --init-rpc.accounts-file=/tmp/accounts.json
```

### 5. Store the wallet password in a file

Because we now only have the wallet password as a value in a k8s secret, we need
to retrieve it and store it in a file that `lnd` can read to auto unlock.

```shell
$ lndinit -v load-secret \
    --source=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.secret-entry-name=wallet-password > /safe/location/walletpassword.txt
```

**Security notice**:

Any process or user that has access to the file system of the container can
potentially read the password if it's stored as a plain file.
For an extra bump in security, a named pipe can be used instead of a file. That
way the password can only be read exactly once from the pipe during `lnd`'s
startup.

```shell
# Create a FIFO pipe first. This will behave like a file except that writes to
# it will only occur once there's a reader on the other end.
$ mkfifo /tmp/wallet-password

# Read the secret from Kubernetes and write it to the pipe. This will only
# return once lnd is actually reading from the pipe. Therefore we need to run
# the command as a background process (using the ampersand notation).
$ lndinit load-secret \
    --source=k8s \
    --k8s.secret-name=lnd-secrets \
    --k8s.secret-entry-name=wallet-password > /tmp/wallet-password &

# Now run lnd and point it to the named pipe.
$ $ lnd \
    --bitcoin.active \
    ...
    --wallet-unlock-password-file=/tmp/wallet-password
```

### 6. Start and auto unlock lnd

With everything prepared, we can now start lnd and instruct it to auto unlock
itself with the password in the file we prepared.

```shell
$ lnd \
    --bitcoin.active \
    ...
    --wallet-unlock-password-file=/safe/location/walletpassword.txt
```

## Logging and idempotent operations

By default, `lndinit` aborts and exits with a zero return code if the desired
result is already achieved (e.g. a secret entry or a wallet database already
exist). This can make it hard to follow exactly what is happening when debugging
the initialization. To assist with debugging, the following two flags can be
used:

- `--verbose (-v)`: Log debug information to `stderr`.
- `--error-on-existing (-e)`: Exit with a non-zero return code (128) if the
  result of an operation already exists. See example below.

**Example**:

```shell
# Treat every non-zero return code as abort condition (default for k8s container
# commands).
$ set -e

# Run the command and catch any non-zero return code in the ret variable. The
# logical OR is required to not fail because of above setting.
$ ret=0
$ lndinit --error-on-existing init-wallet ... || ret=$?
$ if [[ $ret -eq 0 ]]; then
    echo "Successfully initialized wallet."
  elif [[ $ret -eq 128 ]]; then
    echo "Wallet already exists, skipping initialization."
  else
    echo "Failed to initialize wallet!"
    exit 1
  fi
```

## Example `init-wallet-k8s.sh`

```shell
#!/bin/bash

set -e

WALLET_SECRET_NAME=${WALLET_SECRET_NAME:-lnd-wallet-secret}
WALLET_SECRET_NAMESPACE=${WALLET_SECRET_NAMESPACE:-default}
WALLET_DIR=${WALLET_DIR:-~/.lnd/data/chain/bitcoin/mainnet}
WALLET_PASSWORD_FILE=${WALLET_PASSWORD_FILE:-/tmp/wallet-password}
CERT_DIR=${CERT_DIR:-~/.lnd}
UPLOAD_RPC_SECRETS=${UPLOAD_RPC_SECRETS:-0}
RPC_SECRETS_NAME=${RPC_SECRETS_NAME:-lnd-rpc-secret}
RPC_SECRETS_NAMESPACE=${RPC_SECRETS_NAMESPACE:-default}
NETWORK=${NETWORK:-mainnet}
RPC_SERVER=${RPC_SERVER:-localhost:10009}
REMOTE_SIGNING=${REMOTE_SIGNING:0}
REMOTE_SIGNER_RPC_SECRETS_DIR=${REMOTE_SIGNER_RPC_SECRETS_DIR:-/tmp}
REMOTE_SIGNER_RPC_SECRETS_NAME=${REMOTE_SIGNER_RPC_SECRETS_NAME:-lnd-signer-rpc-secret}
REMOTE_SIGNER_RPC_SECRETS_NAMESPACE=${REMOTE_SIGNER_RPC_SECRETS_NAMESPACE:-signer}

echo "[STARTUP] Asserting wallet password exists in secret ${WALLET_SECRET_NAME}"
lndinit gen-password \
  | lndinit -v store-secret \
  --target=k8s \
  --k8s.base64 \
  --k8s.namespace="${WALLET_SECRET_NAMESPACE}" \
  --k8s.secret-name="${WALLET_SECRET_NAME}" \
  --k8s.secret-entry-name=walletpassword

echo ""
echo "[STARTUP] Asserting seed exists in secret ${WALLET_SECRET_NAME}"
lndinit gen-seed \
  | lndinit -v store-secret \
  --target=k8s \
  --k8s.base64 \
  --k8s.namespace="${WALLET_SECRET_NAMESPACE}" \
  --k8s.secret-name="${WALLET_SECRET_NAME}" \
  --k8s.secret-entry-name=walletseed

echo ""
echo "[STARTUP] Asserting wallet is created with values from secret ${WALLET_SECRET_NAME}"
lndinit -v init-wallet \
  --secret-source=k8s \
  --k8s.base64 \
  --k8s.namespace="${WALLET_SECRET_NAMESPACE}" \
  --k8s.secret-name="${WALLET_SECRET_NAME}" \
  --k8s.seed-entry-name=walletseed \
  --k8s.wallet-password-entry-name=walletpassword \
  --init-file.output-wallet-dir="${WALLET_DIR}" \
  --init-file.validate-password

echo ""
echo "[STARTUP] Preparing lnd auto unlock file"

# To make sure the password can be read exactly once (by lnd itself), we create
# a named pipe. Because we can only write to such a pipe if there's a reader on
# the other end, we need to run this in a sub process in the background.
mkfifo "${WALLET_PASSWORD_FILE}"
lndinit -v load-secret \
  --source=k8s \
  --k8s.base64 \
  --k8s.namespace="${WALLET_SECRET_NAMESPACE}" \
  --k8s.secret-name="${WALLET_SECRET_NAME}" \
  --k8s.secret-entry-name=walletpassword > "${WALLET_PASSWORD_FILE}" &

# In case we have a remote signing setup, we also need to provision the RPC
# secrets of the remote signer.
if [[ "${REMOTE_SIGNING}" == "1" ]]; then
  echo "[STARTUP] Provisioning remote signer RPC secrets"
  lndinit -v load-secret \
    --source=k8s \
    --k8s.base64 \
    --k8s.namespace="${REMOTE_SIGNER_RPC_SECRETS_NAMESPACE}" \
    --k8s.secret-name="${REMOTE_SIGNER_RPC_SECRETS_NAME}" \
    --k8s.secret-entry-name=tls.cert > "${REMOTE_SIGNER_RPC_SECRETS_DIR}/tls.cert"
  lndinit -v load-secret \
    --source=k8s \
    --k8s.base64 \
    --k8s.namespace="${REMOTE_SIGNER_RPC_SECRETS_NAMESPACE}" \
    --k8s.secret-name="${REMOTE_SIGNER_RPC_SECRETS_NAME}" \
    --k8s.secret-entry-name=admin.macaroon > "${REMOTE_SIGNER_RPC_SECRETS_DIR}/admin.macaroon"
fi

# In case we want to upload the TLS certificate and macaroons to k8s secrets as
# well once lnd is ready, we can use the wait-ready and store-secret commands in
# combination to wait until lnd is ready and then batch upload the files to k8s.
if [[ "${UPLOAD_RPC_SECRETS}" == "1" ]]; then
  echo ""
  echo "[STARTUP] Starting RPC secret uploader process in background"
  lndinit -v wait-ready \
    && lncli --network "${NETWORK}" --rpcserver "${RPC_SERVER}" \
    --tlscertpath "${CERT_DIR}/tls.cert" \
    --macaroonpath "${WALLET_DIR}/walletkit.macaroon" \
    wallet accounts list > /tmp/accounts.json \
    && lndinit -v store-secret \
    --batch \
    --overwrite \
    --target=k8s \
    --k8s.base64 \
    --k8s.namespace="${RPC_SECRETS_NAMESPACE}" \
    --k8s.secret-name="${RPC_SECRETS_NAME}" \
    "${CERT_DIR}/tls.cert" \
    "${WALLET_DIR}"/*.macaroon \
    /tmp/accounts.json &
fi

# And finally start lnd. We need to use "exec" here to make sure all signals are
# forwarded correctly.
echo ""
echo "[STARTUP] Starting lnd with flags: $@"
exec lnd "$@"
```
