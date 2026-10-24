FROM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS builder

WORKDIR /workdir/
COPY . /workdir/

# The version is not injected: tagpr writes it into version/version.go and the
# release tag carries that commit, so injecting it here would only create a
# second source of truth that can disagree. Only the revision is.
ARG COMMIT=HEAD

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X github.com/tailor-platform/ddflagd/version.Revision=${COMMIT}" \
    -trimpath \
    -o /ddflagd .

# ddflagd needs neither a shell nor a certificate store: it talks to the local
# Datadog Agent over plain HTTP or a Unix socket, and serves plain HTTP itself.
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

COPY --from=builder /ddflagd /ddflagd

# The binary statically links Apache-2.0 dependencies, and section 4 of that
# license applies to object form as much as to source, so the attributions
# travel with the image rather than living only in the repository.
COPY --from=builder /workdir/LICENSE /LICENSE
COPY --from=builder /workdir/CREDITS /CREDITS

# The evaluation listener; the operational listener is 8017.
EXPOSE 8016 8017

USER nonroot:nonroot
ENTRYPOINT ["/ddflagd"]
