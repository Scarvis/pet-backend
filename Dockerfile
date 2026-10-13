# FinanceApp backend — multi-stage build producing a static binary (§9.2).
#
# Stage 1 (builder): compile the Go service with CGO disabled so the binary
# is fully static and runs on the minimal final image.
# Stage 2 (runtime): minimal alpine image with CA certificates (needed for
# outbound TLS: FX API, S3 storage); only the binary is copied over.

FROM golang:1.23-alpine AS builder

WORKDIR /src

# Module metadata first so dependency downloads are layer-cached.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary; no cgo toolchain needed on the final image.
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o /app/server ./cmd/server

FROM alpine:3.20 AS runtime

RUN apk add --no-cache ca-certificates && adduser -D -H appuser
USER appuser

COPY --from=builder /app/server /app/server

EXPOSE 9090

ENTRYPOINT ["/app/server"]
