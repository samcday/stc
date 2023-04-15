#syntax=docker/dockerfile:1.2
FROM golang:1.20-alpine as builder
WORKDIR /usr/src/app
ADD . .
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o stc-csi .

FROM alpine:3.13
RUN apk add --no-cache ca-certificates
COPY --from=builder /usr/src/app/stc-csi /bin/
CMD /bin/stc-csi
