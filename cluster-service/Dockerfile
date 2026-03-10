FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o email-handler .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/email-handler /usr/local/bin/
EXPOSE 8080
CMD ["email-handler"]
