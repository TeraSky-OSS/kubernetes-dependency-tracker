FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/k8s-dependency-tracker

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/k8s-dependency-tracker .
EXPOSE 8080
CMD ["/app/k8s-dependency-tracker"]