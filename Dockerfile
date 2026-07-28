FROM golang:1.26-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o smartrenew .

FROM alpine:3.20
# tzdata is needed so time.LoadLocation works; without it any non-UTC zone
# lookup fails at runtime.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 appuser
WORKDIR /app
COPY --from=builder /app/smartrenew .
USER appuser
EXPOSE 5000
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD wget -qO- http://localhost:5000/api/health || exit 1
CMD ["./smartrenew"]
