FROM golang:1.27.1 AS builder

WORKDIR /workdir/
COPY . /workdir/

# The version is not injected: tagpr writes it into version/version.go and the
# release tag carries that commit, so injecting it here would only create a
# second source of truth that can disagree. Only the revision is.
ARG COMMIT=HEAD

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X github.com/k1LoW/ddflagd/version.Revision=${COMMIT}" \
    -trimpath \
    -o /ddflagd .

# ddflagd needs neither a shell nor a certificate store: it talks to the local
# Datadog Agent over plain HTTP or a Unix socket, and serves plain HTTP itself.
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /ddflagd /ddflagd

# The evaluation listener; the operational listener is 8017.
EXPOSE 8016 8017

USER nonroot:nonroot
ENTRYPOINT ["/ddflagd"]
