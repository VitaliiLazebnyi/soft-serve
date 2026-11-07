FROM alpine:latest

# Create directories
WORKDIR /soft-serve
# Expose data volume
VOLUME /soft-serve

# Environment variables
ENV SOFT_SERVE_DATA_PATH "/soft-serve"
ENV SOFT_SERVE_INITIAL_ADMIN_KEYS ""
# workaround to prevent slowness in docker when running with a tty
ENV CI "1"

# Expose ports
# SSH
EXPOSE 23231/tcp
# HTTP
EXPOSE 23232/tcp
# Stats
EXPOSE 23233/tcp
# Git
EXPOSE 9418/tcp

# Run soft under tini so it is not PID 1. Git starts detached background
# processes (e.g. "git maintenance run --auto --detach" after a push or fetch).
# When the git process that started one exits, the orphan is re-parented to
# PID 1, which must reap it when it exits in turn; tini does, soft would not.
# -s registers tini as a child subreaper, so it still reaps them (and does not
# warn) when the container is started with --init and tini is not PID 1.
ENTRYPOINT [ "/sbin/tini", "-s", "--", "/usr/local/bin/soft", "serve" ]

RUN apk update && apk add --update git bash openssh tini && rm -rf /var/cache/apk/*

# GoReleaser puts the binary for each platform under <os>/<arch>[/<variant>]/.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/soft /usr/local/bin/soft
