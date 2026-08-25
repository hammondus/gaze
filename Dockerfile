# gaze-server only. The TUI and the agent ship as release binaries; the
# server ships as this image and is never a release asset.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off keeps the modernc SQLite driver pure Go and the binary static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /gaze-server ./cmd/gaze-server

FROM alpine:3.20
RUN adduser -D -H gaze && mkdir /data && chown gaze:gaze /data
COPY --from=build /gaze-server /usr/local/bin/gaze-server
USER gaze
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["gaze-server"]
CMD ["-db", "/data/gaze.db", "-addr", ":8080"]
