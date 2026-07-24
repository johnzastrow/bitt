# BitTabby container image (DEPLOY-05).
#
# Two stages, and the result is one static binary on a base with no shell, no
# package manager, and no userland: nothing to exec into, nothing to exploit
# that is not the application itself.
#
# There is no templ generation step here. The generated _templ.go files are
# committed, so the build is a plain `go build` -- no code generator, no network
# beyond the module download, and the image built from a tag is the code at that
# tag rather than whatever the generator produced today.

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
FROM golang:1.26.5-alpine AS build

WORKDIR /src

# Dependencies first, as their own layer: they change far less often than the
# source, so an ordinary code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Provenance, injected the same way the Makefile does it. Both default to
# "unknown", which is the honest answer for a build that did not supply them.
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# CGO off: modernc.org/sqlite is pure Go, so the binary is genuinely static and
# needs no libc in the final image. -trimpath keeps build paths out of it.
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/johnzastrow/bitt/internal/version.Commit=${COMMIT} \
        -X github.com/johnzastrow/bitt/internal/version.Date=${BUILD_DATE}" \
      -o /out/bittabby ./cmd/bittabby

# The data directory is created here, owned by the non-root user, because the
# final image has no shell to mkdir with. A named volume mounted over it
# inherits this ownership; a bind mount does not, which the deploy guide covers.
RUN mkdir -p /data && chown 65532:65532 /data

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
# distroless/static carries CA certificates -- needed for SMTP over TLS and for
# ntfy -- and nothing else. It does not carry zoneinfo, which is why the binary
# embeds time/tzdata (see cmd/bittabby/main.go); period boundaries are computed
# in the instance timezone and must resolve here.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/bittabby /bittabby
COPY --from=build --chown=65532:65532 /data /data

# 65532 is distroless's "nonroot" user. Named numerically as well as by name so
# that a host bind mount can be chowned to a number the operator can see.
USER 65532:65532

ENV BITT_DB_PATH=/data/bitt.db \
    BITT_ADDR=:8080

EXPOSE 8080
VOLUME ["/data"]

# The image has no curl and no shell, so the binary probes itself. See
# healthcheck() in cmd/bittabby/main.go for why that is the right trade.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/bittabby", "--healthcheck"]

ENTRYPOINT ["/bittabby"]
