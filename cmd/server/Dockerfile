FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /server ./cmd/server
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app && mkdir -p /app && chown app:app /app
WORKDIR /app
USER app
COPY --from=build /server /app/server
EXPOSE 8080
ENTRYPOINT ["/app/server"]
