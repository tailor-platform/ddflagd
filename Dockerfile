FROM golang:1.26.1 AS builder

WORKDIR /workdir/
COPY . /workdir/

# The version is not injected: tagpr writes it into version/version.go and the
# release tag carries that commit, so injecting it here would only create a
# second source of truth that can disagree.
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /ddflagd ./cmd/ddflagd

# ddflagd needs neither a shell nor a certificate store: it talks to the local
# Datadog Agent over plain HTTP or a Unix socket, and serves plain HTTP itself.
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /ddflagd /ddflagd

# The evaluation listener; the operational listener is 8017.
EXPOSE 8016 8017

USER nonroot:nonroot
ENTRYPOINT ["/ddflagd"]
