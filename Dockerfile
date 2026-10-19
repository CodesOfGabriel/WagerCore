FROM golang:1.27.2-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -o /out/migrate ./cmd/migrate

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && adduser -D -u 10001 jungle
WORKDIR /app
COPY --from=build /out/server ./server
COPY --from=build /out/migrate ./migrate
USER jungle
EXPOSE 8080
ENTRYPOINT ["/app/server"]
