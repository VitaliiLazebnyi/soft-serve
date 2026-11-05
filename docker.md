# Running Soft Serve Plus with Docker

Soft Serve Plus images are published to the GitHub Container Registry at
[ghcr.io/vitaliilazebnyi/soft-serve-plus][ghcr] for `linux/amd64`,
`linux/arm64` and `linux/arm/v7`.

```sh
docker pull ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
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
  ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
```

Or by using docker-compose:

```yaml
---
version: "3.1"
services:
  soft-serve-plus:
    image: ghcr.io/vitaliilazebnyi/soft-serve-plus:latest
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

## Building the image yourself

The `Dockerfile` copies a prebuilt binary from `<os>/<arch>/soft`, so build that
for Linux first:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o linux/amd64/soft ./cmd/soft
docker build --platform linux/amd64 -t soft-serve-plus:latest .
```

[ghcr]: https://github.com/VitaliiLazebnyi/soft-serve-plus/pkgs/container/soft-serve-plus

> **Warning**
>
> Make sure to run the image without a TTY, i.e.: do not use the `--tty`/`-t`
> flags.
