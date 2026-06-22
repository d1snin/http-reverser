# ==========================================
# Stage 1: Build the Go binary
# ==========================================
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy source code and HTML into the container
COPY . .

# Initialize Go modules if they don't exist, and download dependencies
RUN if [ ! -f go.mod ]; then go mod init reverser; fi
RUN go mod tidy

# Build a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o httpreverser main.go

# ==========================================
# Stage 2: Create the minimal production image
# ==========================================
FROM alpine:latest

WORKDIR /app

# Create a non-root user for security
RUN adduser -D -g '' appuser
USER appuser

# Copy only the compiled binary from the builder stage
# (The index.html is automatically embedded inside this binary by Go)
COPY --from=builder /app/httpreverser .

# Expose the port the app listens on
EXPOSE 7272

# Run the application
CMD ["./httpreverser"]