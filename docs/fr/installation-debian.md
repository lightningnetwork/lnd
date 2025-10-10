# Installation de LND sous Debian/Ubuntu (FR)

Ce guide explique comment installer et lancer **LND** (Lightning Network Daemon) sur Debian/Ubuntu.

---

## 1. Prérequis
- Linux Debian/Ubuntu
- Go 1.20+
- Git
- Accès à un nœud Bitcoin (bitcoind ou btcd)

---

## 2. Installation
```bash
# Installer Go si nécessaire
sudo apt update && sudo apt install golang-go git -y

# Cloner LND
git clone https://github.com/lightningnetwork/lnd.git
cd lnd

# Compiler
make && make install
