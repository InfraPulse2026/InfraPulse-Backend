FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -S backend && adduser -S -G backend backend
WORKDIR /app
COPY --from=build /out/server /app/server
USER backend
EXPOSE 8080 9443
ENTRYPOINT ["/app/server"]
