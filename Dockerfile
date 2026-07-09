FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /commandcode-proxy .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /commandcode-proxy /usr/local/bin/commandcode-proxy
EXPOSE 55990
ENTRYPOINT ["commandcode-proxy"]
CMD ["--host", "0.0.0.0", "--port", "55990"]
