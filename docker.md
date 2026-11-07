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

## Reaping background git processes

Git starts detached background processes (for example
`git maintenance run --auto --detach` after a push or a mirror sync). When they
exit they are re-parented to PID 1 of the container, which must reap them or
they pile up as zombie (`<defunct>`) processes. The image runs Soft Serve Plus
under [tini](https://github.com/krallin/tini) as PID 1 for this reason, so no
extra flags are needed.

If you build your own image with `soft serve` as the entrypoint, or run an older
image that does not include tini, run the container with an init process:
`docker run --init ...`, or `init: true` for the service in docker-compose.

[ghcr]: https://github.com/VitaliiLazebnyi/soft-serve-plus/pkgs/container/soft-serve-plus

> **Warning**
>
> Make sure to run the image without a TTY, i.e.: do not use the `--tty`/`-t`
> flags.
