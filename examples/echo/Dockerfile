# syntax=docker/dockerfile:1
# Build context is the repository root: docker build -f examples/echo/Dockerfile .

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/echo ./examples/echo

FROM alpine:3.21
RUN addgroup -S echo && adduser -S -G echo -H echo
COPY --from=build /out/echo /usr/local/bin/echo
USER echo
EXPOSE 8081
ENTRYPOINT ["echo"]
