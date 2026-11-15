FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dependency definitions
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . ./

# Build statically-linked Go binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o signora ./cmd/server/main.go

# Runtime stage with Node.js for pdf-lib stamper script
FROM node:22-alpine

WORKDIR /app

# Install npm dependencies for PDF stamper
COPY package.json package-lock.json ./
RUN npm install --omit=dev

# Copy compiled binary from builder
COPY --from=builder /app/signora ./signora

# Copy scripts
COPY scripts ./scripts

# Create runtime directories
RUN mkdir -p /app/uploads /app/signed

ENV PORT=8080
EXPOSE 8080

CMD ["./signora"]
