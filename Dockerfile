FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client poppler-utils util-linux && rm -rf /var/lib/apt/lists/*
ARG HUB_UID=1000
ARG HUB_GID=1000
RUN groupadd -g ${HUB_GID} extensions && useradd -u ${HUB_UID} -g ${HUB_GID} -M -d /tmp extensions
ENV HOME=/tmp
COPY bin/ /usr/local/bin/
USER ${HUB_UID}:${HUB_GID}
CMD ["/usr/local/bin/integration-service"]
