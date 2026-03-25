# LND — Lightning Network Daemon

## Project Overview

Complete implementation of a Lightning Network node in Go. Supports channel management, onion-encrypted payments, path finding, and automatic channel management (autopilot). Fully conforms to BOLT specifications 1-11.

## Architecture

### Entry Points

- **`cmd/lnd/`** — Main daemon entry point
- **`cmd/lncli/`** — CLI client for interacting with the daemon
- **`cmd/commands/`** — Shared CLI command definitions

### Core Subsystems

| Package | Purpose |
|---------|---------|
| `lnrpc/` | gRPC/REST service definitions and server implementations |
| `lncfg/` | Configuration parsing and validation |
| `lnwallet/` | Wallet, on-chain transaction management |
| `lnwire/` | Lightning Network wire protocol messages |
| `lnpeer/` | Peer connection management and protocol handling |
| `channeldb/` | Channel state database with migrations |
| `contractcourt/` | Contract enforcement and breach handling |
| `discovery/` | Network graph discovery and gossip |
| `routing/` | Payment path finding and routing |
| `invoices/` | Invoice (BOLT11) creation and management |
| `htlcswitch/` | HTLC forwarding and switching logic |
| `funding/` | Channel funding workflow |
| `autopilot/` | Automatic channel opening |
| `keychain/` | Key derivation and management |
| `brontide/` | Noise protocol transport layer |
| `chainntfns/` | Chain notification interface (btcd, bitcoind, neutrino) |
| `kvdb/` | Key-value database backends (etcd, postgres, sqlite) |
| `graph/` | Channel graph database |
| `shachain/` | SHA chain preimage management |
| `ticker/` | BTC/USD price ticker |
| `walletunlocker/` | Wallet encryption/unlock RPC |
| `subsca/` | Subscription service for client notifications |

### RPC Subpackages

Each has its own proto definitions and service implementations:
`autopilotrpc/`, `chainrpc/`, `devrpc/`, `invoicesrpc/`, `neutrinorpc/`, `peersrpc/`, `routerrpc/`, `signrpc/`, `verrpc/`, `walletrpc/`, `watchtowerrpc/`, `wtclientrpc/`

### Testing

- **`itest/`** — Integration tests (run against live networks)
- **`*/*_test.go`** — Unit tests per package (use `require` library)
- **`contractcourt/testdata/rapid/`** — Property-based tests using `rapid` library

### Build & Infrastructure

- `Makefile` — Build, install, lint, test commands
- `.github/workflows/` — CI/CD (release, lint, integration tests)
- `docker/` — Docker configs for lnd, btcd, bitcoind

## Build & Development

```bash
# Install
make install

# Build
make build

# Run unit tests
go test ./...

# Run a specific package's tests
go test ./lnwire/...

# Run integration tests
go test ./itest/...

# Generate protobuf code (after .proto changes)
make rpc

# Lint
make lint

# Vendor dependencies
make vendor
```

## Technology Stack

- **Language**: Go (1.21+)
- **Protobuf**: gRPC service definitions with REST proxy
- **Database**: kvdb (etcd, PostgreSQL, SQLite)
- **Chain backends**: btcd, Bitcoin Core (bitcoind), Neutrino (light client)
- **Transport**: Noise protocol (brontide)

## Key Dependencies

- `github.com/btcsuite/btcd` — Bitcoin full node implementation
- `github.com/btcsuite/btcwallet` — Wallet implementation
- `google.golang.org/grpc` — gRPC framework
- `google.golang.org/protobuf` — Protocol buffers

## Style Guide

See `.gemini/styleguide.md` for the full Go style guide. Key rules:

- **Line length**: MUST NOT exceed 80 characters (tabs = 8 spaces)
- **Comments**: Explain _why_, not _what_. Every function must be commented.
- **Function comments**: Begin with function name. Exported functions need detailed comments.
- **Tests**: Use `require` library. Prefer table-driven tests or `rapid` property tests.
- **No deprecated stdlib**: Use `slices`, `maps` packages from Go 1.21+.

## Existing AI Assistant Configs

- `.claude/commands/dedupe.md` — Claude Code slash command for finding duplicate issues
- `.gemini/styleguide.md` — Gemini code style guide
- `.gemini/config.yaml` — Gemini project configuration

## Contributing

- Start with code review before creating PRs (see `docs/review.md`)
- Default branch: `master`
- IRC: #lnd on Libera Chat
- MIT licensed

## API Documentation

- Auto-generated API docs: https://api.lightning.community
- Developer guides: https://docs.lightning.engineering
