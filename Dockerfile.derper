FROM alpine:latest
ARG TARGETPLATFORM

RUN apk update && \
    apk upgrade --no-cache && \
    apk add --no-cache ca-certificates &&\
    rm -rf /var/cache/apk/*

COPY /build/output/${TARGETPLATFORM}/derper /usr/bin/derper
WORKDIR /data

ENTRYPOINT [ "/usr/bin/derper" ]