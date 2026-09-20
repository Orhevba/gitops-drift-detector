# Floating major tag on purpose: go.mod's `go` line moves whenever a dependency
# bump needs a newer Go (a pinned 1.22 here broke the build once that happened).
FROM golang:1 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/manager ./cmd/manager

# The controller shells out to `git` to clone watched repos, so the final
# image needs it installed too — a from-scratch/distroless image won't work.
FROM alpine:3.20
RUN apk add --no-cache git ca-certificates
COPY --from=builder /out/manager /manager
ENTRYPOINT ["/manager"]
