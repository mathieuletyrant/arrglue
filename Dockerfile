FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /arrglue ./cmd/arrglue

FROM scratch
COPY --from=build /arrglue /arrglue
ENTRYPOINT ["/arrglue"]
