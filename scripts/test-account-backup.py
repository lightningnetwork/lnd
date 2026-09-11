#!/usr/bin/env python3
"""Local regtest wallet-loss drill; needs built LND and Bitcoin Core binaries.

Uses temporary wallets and loopback listeners. Never reads user credentials or
prints generated seed material. LND_BINARY and BITCOIND_BINARY select binaries.
"""
import base64
import json
import os
import pathlib
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.request


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for(check, message, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except (OSError, ValueError):
            pass
        time.sleep(0.2)
    raise RuntimeError(message)


class Drill:
    def __init__(self, root):
        self.root = pathlib.Path(root)
        self.bitcoin = self.root / "bitcoin"
        self.bitcoin.mkdir()
        self.wallet = self.root / "lnd"
        self.backup = self.root / "backup"
        self.backup.mkdir()
        self.backup_file = self.backup / "accounts.json"
        self.password = self.root / "password"
        self.password.write_text("local-regtest-password")
        self.password.chmod(0o600)
        self.rpc, self.zblock, self.ztx = port(), port(), port()
        self.rest, self.grpc, self.peer = port(), port(), port()
        self.node = None
        self.miner = None
        self.logs = []
        self.mnemonic = None

    def launch(self, command, name):
        log = open(self.root / name, "ab")
        self.logs.append(log)
        return subprocess.Popen(command, stdout=log, stderr=log)

    def btc(self, method, *params, wallet=False):
        cookie = b"recoverytest:local-regtest-rpc-password"
        url = f"http://127.0.0.1:{self.rpc}" + ("/wallet/miner" if wallet else "")
        req = urllib.request.Request(url, json.dumps({"jsonrpc": "1.0", "id": "drill", "method": method, "params": params}).encode())
        req.add_header("Authorization", "Basic " + base64.b64encode(cookie).decode())
        with urllib.request.urlopen(req, timeout=15) as response:
            result = json.load(response)
        if result.get("error"):
            raise RuntimeError("local Bitcoin RPC failed")
        return result["result"]

    def api(self, route, data=None):
        cert = self.wallet / "tls.cert"
        context = ssl.create_default_context(cafile=str(cert))
        req = urllib.request.Request(f"https://127.0.0.1:{self.rest}" + route, None if data is None else json.dumps(data).encode())
        req.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(req, context=context, timeout=20) as response:
            return json.load(response)

    def mine(self, count):
        target = self.btc("getnewaddress", wallet=True)
        self.btc("generatetoaddress", count, target)

    def start(self, *, protected=False, create=False, rescan=False, fresh=False, expect_failure=False):
        if fresh:
            shutil.rmtree(self.wallet, ignore_errors=True)
        self.wallet.mkdir(exist_ok=True)
        command = [os.environ.get("LND_BINARY", "lnd"), f"--lnddir={self.wallet}", "--bitcoin.regtest", "--bitcoin.node=bitcoind", f"--bitcoind.rpchost=127.0.0.1:{self.rpc}", "--bitcoind.rpcuser=recoverytest", "--bitcoind.rpcpass=local-regtest-rpc-password", f"--bitcoind.zmqpubrawblock=tcp://127.0.0.1:{self.zblock}", f"--bitcoind.zmqpubrawtx=tcp://127.0.0.1:{self.ztx}", f"--restlisten=127.0.0.1:{self.rest}", f"--rpclisten=127.0.0.1:{self.grpc}", f"--listen=127.0.0.1:{self.peer}", "--no-macaroons", "--nobootstrap", f"--wallet-unlock-password-file={self.password}", "--wallet-unlock-allow-create", "--debuglevel=warn"]
        if protected:
            command.append(f"--wallet-account-backup={self.backup_file}")
        if create:
            command.append("--wallet-account-backup-create")
        if rescan:
            command.append("--reset-wallet-transactions")
        self.node = self.launch(command, "lnd.log")
        if expect_failure:
            assert self.node.wait(timeout=30) != 0, "incomplete restoration admitted"
            self.node = None
            return
        if fresh:
            seed = wait_for(lambda: self.api("/v1/genseed"), "seed RPC unavailable")
            if self.mnemonic is None:
                self.mnemonic = seed["cipher_seed_mnemonic"]
            # The seed remains in memory and the ephemeral wallet only.
            self.api("/v1/initwallet", {"wallet_password": base64.b64encode(self.password.read_bytes()).decode(), "cipher_seed_mnemonic": self.mnemonic, "recovery_window": 0})
        wait_for(lambda: self.api("/v1/getinfo").get("synced_to_chain"), "LND failed to synchronize")

    def stop(self):
        if self.node:
            self.node.terminate()
            self.node.wait(timeout=30)
            self.node = None

    def create_account(self, name):
        self.api("/v2/wallet/accounts/create", {"name": name, "address_type": "TAPROOT_PUBKEY", "i_know_what_i_am_doing": True})

    def next_address(self, change=False):
        return self.api("/v2/wallet/address/next", {"account": "treasury", "type": "TAPROOT_PUBKEY", "change": change})["addr"]

    def balance(self):
        return int(self.api("/v1/balance/blockchain?account=treasury")["confirmed_balance"])

    def account(self):
        return self.api("/v2/wallet/accounts?name=treasury")["accounts"][0]

    def spend(self, sats):
        target = self.btc("getnewaddress", wallet=True)
        funded = self.api("/v2/wallet/psbt/fund", {"raw": {"outputs": {target: str(sats)}}, "account": "treasury", "sat_per_vbyte": "10", "min_confs": 1})
        snapshot = json.loads(self.backup_file.read_text())
        record = next(a for a in snapshot["accounts"] if a["name"] == "treasury")
        assert record["internal_key_count"] >= int(self.account()["internal_key_count"]), "change not durable before funding returned"
        signed = self.api("/v2/wallet/psbt/finalize", {"funded_psbt": funded["funded_psbt"], "account": "treasury"})
        self.api("/v2/wallet/tx", {"tx_hex": signed["raw_final_tx"]})
        self.mine(1)

    def run(self):
        self.miner = self.launch([os.environ.get("BITCOIND_BINARY", "bitcoind"), f"-datadir={self.bitcoin}", "-regtest", "-server", "-listen=0", "-discover=0", "-dnsseed=0", f"-rpcport={self.rpc}", "-rpcuser=recoverytest", "-rpcpassword=local-regtest-rpc-password", f"-zmqpubrawblock=tcp://127.0.0.1:{self.zblock}", f"-zmqpubrawtx=tcp://127.0.0.1:{self.ztx}", "-fallbackfee=0.0002"], "bitcoin.log")
        wait_for(lambda: self.btc("getblockchaininfo"), "Bitcoin Core startup failed")
        self.btc("createwallet", "miner")
        self.mine(101)
        self.start(protected=True, create=True, fresh=True)
        self.create_account("earlier")
        self.create_account("treasury")
        target = self.next_address()
        self.btc("sendtoaddress", target, 0.01, wallet=True)
        self.mine(1)
        wait_for(lambda: self.balance() == 1000000, "initial deposit missing")
        self.spend(200000)
        wait_for(lambda: 0 < self.balance() < 800000, "internal change missing")
        expected = self.balance()
        original = self.account()
        saved = json.loads(self.backup_file.read_text())
        record = next(a for a in saved["accounts"] if a["name"] == "treasury")
        assert record["account_index"] == 2 and record["internal_key_count"] > 0
        self.stop()
        # Destroy the complete wallet data directory. Only the seed and the
        # independent public account record survive into reconstruction.
        self.start(fresh=True)
        self.create_account("earlier")
        self.create_account("treasury")
        reconstructed = self.account()
        assert reconstructed["derivation_path"] == original["derivation_path"]
        assert reconstructed["extended_public_key"] == original["extended_public_key"]
        for _ in range(record["external_key_count"]):
            self.next_address()
        self.stop()
        self.start(protected=True, expect_failure=True)
        print("PASS startup gate: incomplete internal reconstruction refused", flush=True)
        self.start(rescan=True)
        assert self.balance() == 0, "negative control unexpectedly recovered internal change"
        print(f"PASS negative control: missing internal branch recovers 0 of {expected} sat", flush=True)
        for _ in range(record["internal_key_count"]):
            self.next_address(change=True)
        self.stop()
        self.start(protected=True, rescan=True)
        wait_for(lambda: self.balance() == expected, "full reconstruction missed change")
        self.spend(100000)
        wait_for(lambda: 0 < self.balance() < expected, "recovered change could not be spent")
        print("PASS seed restore: correct account index, both branches, rescan, and confirmed spend", flush=True)

    def close(self):
        self.stop()
        if self.miner:
            self.miner.terminate()
            self.miner.wait(timeout=30)
        for log in self.logs:
            log.close()


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="lnd-account-drill-") as root:
        drill = Drill(root)
        try:
            drill.run()
        finally:
            drill.close()
