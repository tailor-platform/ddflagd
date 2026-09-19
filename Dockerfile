FROM golang:1.27.1@sha256:1cfcdb11f37fce9429f617100f39e0251748bbaba454bd431275155701765058 AS builder

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
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

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
