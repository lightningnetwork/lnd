# Understanding Sphinx Replay Protection

Sphinx is a protocol enabling anonymous and untraceable messaging in networks, commonly used in Lightning's onion routing. To prevent replay attacks, a Sphinx replay database stores message hashes along with metadata (e.g., timestamp, sender ID).

## How It Works
1. **Message Check**: When a message is received, its hash is compared against stored entries.
2. **Replay Prevention**: If the message exists, it is discarded; otherwise, it is added to the database.
3. **Network Integrity**: This ensures that previously sent messages cannot be resent maliciously.

## Onion Routing in Lightning
Sphinx follows source-based onion routing, where messages are encrypted in nested layers. Each node peels one layer, revealing only the next hop, ensuring sender anonymity and path secrecy.

Lightning’s **Sphinx Mix Format** differs from Tor, offering enhanced security tailored to payments.

For deeper insights, refer to [Chapter 10 of the Lightning Network Book](https://github.com/lnbook/lnbook/blob/develop/10_onion_routing.asciidoc).

