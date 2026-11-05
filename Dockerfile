FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /proxy ./cmd/proxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /proxy /proxy
USER nonroot:nonroot
EXPOSE 8088
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/proxy", "-config", "/config/config.json", "-healthcheck"]
ENTRYPOINT ["/proxy", "-config", "/config/config.json"]
