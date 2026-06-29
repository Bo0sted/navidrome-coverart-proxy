FROM golang:1.23-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /navidrome-coverart-proxy .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /navidrome-coverart-proxy /navidrome-coverart-proxy
USER 65534:65534
ENTRYPOINT ["/navidrome-coverart-proxy"]
