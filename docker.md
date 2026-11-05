# Running Soft Serve Plus with Docker

There are no published Soft Serve Plus images yet. The `charmcli/soft-serve`
and `ghcr.io/charmbracelet/soft-serve` images contain the original Soft Serve,
without the changes in this fork.

Build the image from a checkout of this repository. The `Dockerfile` copies a
prebuilt `soft` binary, so build that for Linux first:

```sh
CGO_ENABLED=0 GOOS=linux go build -o soft ./cmd/soft
docker build -t soft-serve-plus:latest .
```

Here’s how you might run Soft Serve Plus as a container.  Keep in mind that
repositories are stored in the `/soft-serve` directory, so you’ll likely want
to mount that directory as a volume in order keep your repositories backed up.

```sh
docker run \
  --name=soft-serve-plus \
  --volume /path/to/data:/soft-serve \
  --publish 23231:23231 \
  --publish 23232:23232 \
  --publish 23233:23233 \
  --publish 9418:9418 \
  -e SOFT_SERVE_INITIAL_ADMIN_KEYS="YOUR_ADMIN_KEY_HERE" \
  --restart unless-stopped \
  soft-serve-plus:latest
```

Or by using docker-compose:

```yaml
---
version: "3.1"
services:
  soft-serve-plus:
    image: soft-serve-plus:latest
    container_name: soft-serve-plus
    volumes:
      - /path/to/data:/soft-serve
    ports:
      - 23231:23231
      - 23232:23232
      - 23233:23233
      - 9418:9418
    environment:
      SOFT_SERVE_INITIAL_ADMIN_KEYS: "YOUR_ADMIN_KEY_HERE"
    restart: unless-stopped
```

[docker]: https://hub.docker.com/r/charmcli/soft-serve
[ghcr]: https://github.com/charmbracelet/soft-serve/pkgs/container/soft-serve


> **Warning**
>
> Make sure to run the image without a TTY, i.e.: do not use the `--tty`/`-t`
> flags.


***

Part of [Charm](https://charm.sh).

<a href="https://charm.sh/"><img alt="The Charm logo" src="https://stuff.charm.sh/charm-badge-unrounded.jpg" width="400"></a>

Charm热爱开源 • Charm loves open source
