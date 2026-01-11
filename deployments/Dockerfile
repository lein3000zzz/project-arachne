FROM golang:1.25.1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o limitfinder ./cmd/limitfinder

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/limitfinder .
COPY --from=builder /app/tools/lf.env ./tools/lf.env
CMD ["./limitfinder"]