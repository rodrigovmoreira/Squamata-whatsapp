# Build estático: o binário de runtime não carrega toolchain nem compilador C.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/squamata-whatsapp ./cmd/squamata-whatsapp

# Runtime distroless: sem shell, sem curl, sem Go — só o binário.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/squamata-whatsapp /squamata-whatsapp

ENV SQUAMATA_WHATSAPP_DB_PATH=/data/whatsapp.db \
    SQUAMATA_WHATSAPP_PORT=3006

EXPOSE 3006

USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/squamata-whatsapp", "-health"]

ENTRYPOINT ["/squamata-whatsapp"]
