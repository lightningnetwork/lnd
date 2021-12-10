# Init

Sample configuration files for:

```
systemd: lnd.service
```

## systemd

Add the example `lnd.service` file to `/etc/systemd/system/` and modify it according to your system and user configuration. Use the following commands to interact with the service:

```bash
# Enable lnd to automatically start on system boot
systemctl enable lnd

# Start lnd
systemctl start lnd

# Restart lnd
systemctl restart lnd

# Stop lnd
systemctl stop lnd
```

Systemd will attempt to restart lnd automatically if it crashes or otherwise stops unexpectedly.
