# ----------------------
# Builder stage
# ----------------------
FROM golang:1.25-alpine AS builder
WORKDIR /app

# Install git (openssl not needed if you’re not building HTTPS inside)
RUN apk add --no-cache git

# Copy Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the binary
RUN go build -trimpath -buildvcs=false -o nordik-drive-api ./cmd/server && \
    go build -trimpath -buildvcs=false -o nordik-gateway ./cmd/gateway

# Local/CI verification only; never deployed as an application image.
FROM builder AS verification
RUN apk add --no-cache docker-cli build-base
CMD ["go", "test", "./..."]

# ----------------------
# Final stage
# ----------------------
FROM alpine:3.22 AS runtime
WORKDIR /app

# Install CA certificates (for outbound HTTPS requests to other services)
RUN apk add --no-cache ca-certificates tzdata

# Copy the binary from builder
FROM runtime AS gateway
COPY --from=builder /app/nordik-gateway /app/nordik-gateway
ENTRYPOINT ["/app/nordik-gateway"]

FROM runtime AS server
COPY --from=builder /app/nordik-drive-api .

# Expose Cloud Run HTTP port
EXPOSE 8080

# Run the server (Cloud Run expects it to bind to $PORT or 8080)
CMD ["./nordik-drive-api"]
