FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o search ./cmd/search && go build -o rank ./cmd/rank && go build -o user ./cmd/user

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/search /app/rank /app/user ./
COPY --from=builder /app/configs ./configs
CMD ["./search"]
