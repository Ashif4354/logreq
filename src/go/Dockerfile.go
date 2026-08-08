# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy module definition
COPY go.mod ./

# Copy Go source
COPY main.go ./

# Build optimized static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o logreq .

# Final minimal runtime image
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/logreq /app/logreq
COPY .env.example ./

EXPOSE 8081

ENTRYPOINT ["/app/logreq", "-host", "0.0.0.0", "-port", "8081"]
