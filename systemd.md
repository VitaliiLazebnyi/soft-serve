# Running Soft Serve Plus as a Systemd Service

Most Linux OSes use Systemd as an init system and service management. You can
use Systemd to manage Soft Serve Plus as a service on your host machine.

The upstream Soft Serve deb/rpm packages come with Systemd service files
pre-packaged, but they install the original Soft Serve, not this fork. To run
Soft Serve Plus, [build it from source](https://github.com/VitaliiLazebnyi/soft-serve-plus#installation),
install the `soft` binary as `/usr/bin/soft`, and follow this guide.

## Writing a Systemd Service File

> **Note** the upstream deb/rpm packages ship this service file already, but
> they install the original Soft Serve. For Soft Serve Plus, write it yourself.

Start by writing a Systemd service file to define how your Soft Serve Plus server
should start.

First, we need to specify where the data should live for our server. Here I
will be choosing `/var/local/lib/soft-serve` to store the server's data. Soft
Serve will look for this path in the `SOFT_SERVE_DATA_PATH` environment
variable.

Make sure this directory exists before proceeding.

```sh
sudo mkdir -p /var/local/lib/soft-serve
```

We will also create a `/etc/soft-serve.conf` file for any extra server settings that we want to override.

```conf
# Config defined here will override the config in /var/local/lib/soft-serve/config.yaml
# Keys defined in `SOFT_SERVE_INITIAL_ADMIN_KEYS` will be merged with
# the `initial_admin_keys` from /var/local/lib/soft-serve/config.yaml.
#
#SOFT_SERVE_GIT_LISTEN_ADDR=:9418
#SOFT_SERVE_HTTP_LISTEN_ADDR=:23232
#SOFT_SERVE_SSH_LISTEN_ADDR=:23231
#SOFT_SERVE_SSH_KEY_PATH=ssh/soft_serve_host_ed25519
#SOFT_SERVE_INITIAL_ADMIN_KEYS='ssh-ed25519 AAAAC3NzaC1lZDI1...'
```

> **Note** Soft Serve Plus stores its server configuration and settings in
> `config.yaml` under its _data path_ directory specified using
> `SOFT_SERVE_DATA_PATH` environment variable.

Now, let's write a new `/etc/systemd/system/soft-serve.service` Systemd service file:

```conf
[Unit]
Description=Soft Serve Plus git server 🍦
Documentation=https://github.com/VitaliiLazebnyi/soft-serve-plus
Requires=network-online.target
After=network-online.target

[Service]
Type=simple
Restart=always
RestartSec=1
ExecStart=/usr/bin/soft serve
Environment=SOFT_SERVE_DATA_PATH=/var/local/lib/soft-serve
EnvironmentFile=-/etc/soft-serve.conf
WorkingDirectory=/var/local/lib/soft-serve

[Install]
WantedBy=multi-user.target
```

Great, we now have a Systemd service file for Soft Serve Plus. The settings defined
here may vary depending on your specific setup. This assumes that you want to
run Soft Serve Plus as `root`. For more information on Systemd service files, refer
to
[systemd.service](https://www.freedesktop.org/software/systemd/man/systemd.service.html)

## Start Soft Serve Plus on boot

Now that we have our Soft Serve Plus Systemd service file in-place, let's go ahead
and enable and start Soft Serve Plus to run on-boot.

```sh
# Reload systemd daemon
sudo systemctl daemon-reload
# Enable Soft Serve Plus to start on-boot
sudo systemctl enable soft-serve.service
# Start Soft Serve Plus now!!
sudo systemctl start soft-serve.service
```

You can monitor the server logs using `journalctl -u soft-serve.service`. Use
`-f` to _tail_ and follow the logs as they get written.

***

Part of [Charm](https://charm.sh).

<a href="https://charm.sh/"><img alt="The Charm logo" src="https://stuff.charm.sh/charm-badge-unrounded.jpg" width="400"></a>

Charm热爱开源 • Charm loves open source
