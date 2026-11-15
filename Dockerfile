FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dependency definitions
COPY backend/go.mod backend/go.sum ./
RUN go mod download

# Copy backend source code
COPY backend/ ./

# Build statically-linked Go binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o signora ./cmd/server/main.go

# Runtime stage with Node.js for pdf-lib stamper script
FROM node:22-alpine

WORKDIR /app

# Install npm dependencies for PDF stamper
COPY backend/package.json backend/package-lock.json ./
RUN npm install --omit=dev

# Copy compiled binary from builder
COPY --from=builder /app/signora ./signora

# Copy backend scripts
COPY backend/scripts ./scripts
COPY backend/scripts ./backend/scripts

# Create runtime directories
RUN mkdir -p /app/uploads /app/signed

ENV PORT=8080
EXPOSE 8080

CMD ["./signora"]
