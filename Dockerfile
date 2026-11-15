# Go のバージョンは go.mod の go directive とそろえる（test.yaml の go-version-guard が検査する）。
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# バージョンは release-please が更新する manifest から読む。release の commit をビルドすれば、その版が入る。
RUN version="$(sed -n 's/.*"\.": *"\([^"]*\)".*/\1/p' .release-please-manifest.json)" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${version:-dev}" -o /out/navidrome-curator ./cmd/navidrome-curator

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/navidrome-curator /usr/local/bin/navidrome-curator
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/navidrome-curator"]
