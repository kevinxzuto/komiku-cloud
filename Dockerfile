FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/komiku-cloud .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/komiku-cloud /app/komiku-cloud
VOLUME ["/data"]
ENV DB_PATH=/data/komiku-cloud.db
ENV LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/app/komiku-cloud"]
