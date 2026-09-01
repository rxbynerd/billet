# syntax=docker/dockerfile:1
FROM docker.io/library/golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/billet ./cmd/billet
# Pre-create the bolt backend's conventional data directory, owned by the
# runtime user, so a volume or tmpfs mounted there (and the read-only
# root filesystem around it) needs no further setup.
RUN mkdir -p /out/var/lib/billet

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/billet /usr/local/bin/billet
COPY --from=build --chown=65532:65532 /out/var/lib/billet /var/lib/billet

# 8140 MCP Streamable HTTP, 8141 Connect RPC.
EXPOSE 8140 8141
# Numeric, not the "nonroot" name: a Pod with runAsNonRoot cannot verify
# a non-numeric image user and refuses to start the container.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/billet"]
# The loopback-only default listen address is unreachable from outside a
# container; bind the container interface so published ports work.
CMD ["serve", "--listen=:8140"]
