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

# Run soft under tini so it is not PID 1. Git detaches background children
# (e.g. "git maintenance run --auto --detach" after a push or fetch) that are
# re-parented to PID 1 when they finish; tini reaps them, soft would not.
ENTRYPOINT [ "/sbin/tini", "--", "/usr/local/bin/soft", "serve" ]

RUN apk update && apk add --update git bash openssh tini && rm -rf /var/cache/apk/*

# GoReleaser puts the binary for each platform under <os>/<arch>[/<variant>]/.
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/soft /usr/local/bin/soft
