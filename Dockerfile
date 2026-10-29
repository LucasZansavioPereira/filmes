FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /filmes .

FROM alpine:3.22
# ffmpeg tira as capas e lê a duração dos vídeos.
RUN apk add --no-cache tzdata ffmpeg
COPY --from=build /filmes /usr/local/bin/filmes
ENV FILMES_DATA=/data FILMES_PASTA=/filmes FILMES_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["filmes"]
