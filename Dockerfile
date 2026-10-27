# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gatekeeper ./cmd/gatekeeper

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && addgroup -S gatekeeper && adduser -S -G gatekeeper -H gatekeeper
COPY --from=build /out/gatekeeper /usr/local/bin/gatekeeper
COPY examples/gatekeeper.json /etc/gatekeeper/gatekeeper.json
USER gatekeeper
EXPOSE 8080 9090
ENTRYPOINT ["gatekeeper"]
CMD ["-config", "/etc/gatekeeper/gatekeeper.json"]
