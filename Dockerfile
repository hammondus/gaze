# gaze-server only. The TUI and the agent ship as release binaries; the
# server ships as this image and is never a release asset.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off keeps the modernc SQLite driver pure Go and the binary static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/gaze-server ./cmd/gaze-server

# Pre-create the database directory here, with the runtime uid. The runtime
# stage has no shell to mkdir or chown with. 0700 because nothing but the
# server has any business reading the database.
RUN mkdir -m 0700 /out/data && chown 65532:65532 /out/data

# distroless/static, not alpine. The binary is static, so the base contributes
# no code — only files. It needs three of them: the CA roots that alert mail
# over SMTP+TLS verifies against, tzdata, and a passwd entry for the nonroot
# uid. distroless ships all three, and no shell, libc, or package manager.
#
# The Debian release is named in the tag deliberately. The floating
# `static:nonroot` tag follows Google onto the next Debian major without this
# file changing; naming it here makes that a commit instead of a surprise.
# The tag still floats *within* Debian 13, so a rebuild picks up CA and tzdata
# updates — which is why this is a tag and not a digest.
FROM gcr.io/distroless/static-debian13:nonroot

# /usr/local/bin is on the image's PATH, so the runbook in compose.yml
# (`docker compose exec gaze-server gaze-server enroll …`) keeps working with
# no shell in the image: docker resolves the command against PATH and execs it.
COPY --from=build /out/gaze-server /usr/local/bin/gaze-server

# /data must exist in the image owned by nonroot. A fresh named volume takes
# the ownership of the image directory it mounts over, and a root-owned volume
# is not writable by uid 65532 — which surfaces as "unable to open database
# file" on first start, not as a permission error.
COPY --from=build --chown=nonroot:nonroot /out/data /data

USER nonroot:nonroot
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gaze-server"]
CMD ["-db", "/data/gaze.db", "-addr", ":8080"]
