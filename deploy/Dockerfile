# PanRouter 单二进制构建(M1:纯 Go SQLite,无 CGO)
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/panrouter ./cmd/panrouter

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S panrouter && adduser -S panrouter -G panrouter
WORKDIR /app
COPY --from=build /out/panrouter /app/panrouter
USER panrouter
VOLUME /app/data
EXPOSE 6400
ENTRYPOINT ["/app/panrouter"]
