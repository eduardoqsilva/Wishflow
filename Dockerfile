FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /tome-wishlist-downloader .

FROM alpine:latest

RUN apk --no-cache add ca-certificates
WORKDIR /app

COPY --from=builder /tome-wishlist-downloader .

CMD ["./tome-wishlist-downloader"]