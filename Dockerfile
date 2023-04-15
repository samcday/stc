#syntax=docker/dockerfile:1.2
FROM --platform=$BUILDPLATFORM golang:1.20 as builder
WORKDIR /usr/src/app
ADD . .
ENV GOTRACEBACK=all
ARG SKAFFOLD_GO_GCFLAGS
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -gcflags="${SKAFFOLD_GO_GCFLAGS}" -v -o stc .

FROM alpine:3.13
RUN apk add --no-cache ca-certificates
COPY --from=builder /usr/src/app/stc /bin/
CMD /bin/stc
