FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server
RUN CGO_ENABLED=0 go build -trimpath -o /out/backfill-logs ./cmd/backfill-logs
RUN CGO_ENABLED=0 go build -trimpath -o /out/access ./cmd/access
RUN CGO_ENABLED=0 go build -trimpath -o /out/pki ./cmd/pki

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -S -g 10001 backend && adduser -S -u 10001 -G backend backend
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/backfill-logs /app/backfill-logs
COPY --from=build /out/access /app/access
COPY --from=build /out/pki /app/pki
USER backend
EXPOSE 8080 9443 9444
ENTRYPOINT ["/app/server"]
