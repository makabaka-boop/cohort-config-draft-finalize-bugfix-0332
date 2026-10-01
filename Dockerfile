# Build environment shared by the API and verify images.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# API binary
FROM build AS build-api
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api

# One-shot verify binary (unit tests run as a build gate; PostgreSQL
# integration tests self-skip without TEST_DATABASE_URL).
FROM build AS build-verify
RUN CGO_ENABLED=0 go test ./... -count=1 && CGO_ENABLED=0 go build -o /out/verify ./cmd/verify

# Runtime image for the API
FROM debian:bookworm-slim AS api
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --create-home --shell /usr/sbin/nologin appuser
COPY --from=build-api /out/api /usr/local/bin/api
ENV LISTEN_ADDR=:8080
EXPOSE 8080
USER appuser
ENTRYPOINT ["api"]

# Verify runs in a minimal image with CA certificates present.
FROM debian:bookworm-slim AS verify
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 65532 --create-home --shell /usr/sbin/nologin verifier
COPY --from=build-verify /out/verify /usr/local/bin/verify
USER verifier
ENTRYPOINT ["verify"]
