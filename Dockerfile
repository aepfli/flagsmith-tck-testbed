# Same multi-stage shape as flagd-testbed: take the vendor's own image as the base and add a
# launchpad binary as PID 1. The launchpad supervises the vendor process, which keeps POST /stop
# meaning "kill the backend process" rather than "stop the container" -- the control API requires
# that, because dynamically mapped host ports do not survive a container restart.

FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY launchpad/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /launchpad .

FROM flagsmith/edge-proxy:latest
USER root
COPY --from=builder /launchpad /launchpad
COPY flags/ /flags/
COPY launchpad/configs/ /configs/
# The launchpad renders the Edge Proxy's config.json at startup, so /app must be writable by the
# unprivileged user the base image runs as.
RUN mkdir -p /app && chown -R nobody /app /flags /configs
USER nobody
# 8000: Edge Proxy (what the provider under test talks to)
# 8080: launchpad -- control API, and the environment-document endpoint the proxy polls
EXPOSE 8000 8080
ENTRYPOINT ["/launchpad"]
