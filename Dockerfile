FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88

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
