FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/ third_party/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /webhook .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /webhook /webhook
USER 65532:65532
ENTRYPOINT ["/webhook"]
