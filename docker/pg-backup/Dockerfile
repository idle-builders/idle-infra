FROM postgres:18.4-alpine3.22

RUN apk add --no-cache aws-cli openssl coreutils \
    && rm -rf /var/cache/apk/*

USER 65534:65534
