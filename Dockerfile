FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /rpc-health .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /rpc-health /rpc-health
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/rpc-health", "-healthcheck"]
ENTRYPOINT ["/rpc-health"]
