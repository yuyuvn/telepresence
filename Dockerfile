# Dockerfile with telepresence and its prerequisites
FROM alpine:3.13

# Install Telepresence prerequisites
RUN apk add --no-cache curl iproute2 sshfs docker

# Download and install the telepresence binary
COPY ./build-output/bin/telepresence .
ENV TELEPRESENCE_REGISTRY j14a
RUN install -o root -g root -m 0755 telepresence /usr/local/bin/telepresence
