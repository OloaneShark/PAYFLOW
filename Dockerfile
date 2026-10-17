# Build: docker build --target api|worker|migrate|admin -t payflow-<target>:local .
FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/admin ./cmd/admin \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
COPY certs/rds-global-bundle.pem /etc/ssl/certs/rds-global-bundle.pem
COPY --from=build /out/healthcheck /app/healthcheck
USER 65532:65532

FROM runtime AS api
COPY --from=build /out/api /app/api
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s CMD ["/app/healthcheck","http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/app/api"]

FROM runtime AS worker
COPY --from=build /out/worker /app/worker
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s CMD ["/app/healthcheck","http://127.0.0.1:9091/readyz"]
ENTRYPOINT ["/app/worker"]

FROM runtime AS migrate
COPY --from=build /out/migrate /app/migrate
ENTRYPOINT ["/app/migrate"]

FROM runtime AS admin
COPY --from=build /out/admin /app/admin
ENTRYPOINT ["/app/admin"]
