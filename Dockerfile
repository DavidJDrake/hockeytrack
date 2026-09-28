# Base images are pinned by digest so a build is reproducible and a tag cannot
# be silently repointed. The tag is kept for readability only. To bump: run
# `docker buildx imagetools inspect <image>:<tag>` and copy the top-level
# (multi-arch index) Digest line.
FROM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /ingestor ./cmd/ingestor

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /ingestor /ingestor
ENTRYPOINT ["/ingestor"]
