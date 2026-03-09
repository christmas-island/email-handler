FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY main.go .
RUN go mod init email-handler && go build -o email-handler .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/email-handler /usr/local/bin/
EXPOSE 8080
CMD ["email-handler"]
