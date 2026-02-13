# Build stage
FROM golang:1.25.7-alpine AS builder

WORKDIR /app

# Install git and certificates
RUN apk add --no-cache git ca-certificates

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o binternet-go cmd/server/main.go

# Final stage
FROM gcr.io/distroless/static-debian11

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/binternet-go .

# Expose port
EXPOSE 8021

# Helper to run the executable
ENTRYPOINT ["/app/binternet-go"]
