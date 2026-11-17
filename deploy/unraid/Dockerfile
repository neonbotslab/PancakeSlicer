FROM pancakeslicer-unraid-base:20261116
ARG REVISION
ARG SOURCE_SHA256
LABEL org.opencontainers.image.version="${REVISION}" org.opencontainers.image.revision="${REVISION}" org.opencontainers.image.source="https://github.com/ZimengXiong/PancakeSlicer"
COPY orca-webapp-server /app/engine/orca-webapp-server
COPY web/ /app/web/
COPY source.tar.gz /app/engine/source.tar.gz
ENV ORCA_BUILD_REVISION=${REVISION} ORCA_BUILD_SOURCE_SHA256=${SOURCE_SHA256} ORCA_BUILD_IMAGE=pancakeslicer-unraid:${REVISION}
