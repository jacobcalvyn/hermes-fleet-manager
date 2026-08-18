# Pin by digest so Hermes version bumps reuse the apt/uv layers. Bump this
# digest only when the OS/toolchain base must change.
FROM node:26-bookworm-slim@sha256:cd565714d4da3e84bfd341e31448f81d47c6362198f152345297c9c1154e6341

ARG HERMES_REPO=https://github.com/NousResearch/hermes-agent.git
ARG HERMES_REF=f80f453ae0679347e38abc917c7f94f717bf96c5
ARG HERMES_VERSION=0.20.1
ARG RUNTIME_BUILD_ID=unknown

ENV PIP_DEFAULT_TIMEOUT=180 \
    PIP_RETRIES=10 \
    UV_HTTP_TIMEOUT=180

RUN apt-get -o APT::Sandbox::User=root update \
    && apt-get -o APT::Sandbox::User=root install -y --no-install-recommends \
        bash \
        build-essential \
        ca-certificates \
        curl \
        ffmpeg \
        fonts-freefont-ttf \
        fonts-ipafont-gothic \
        fonts-liberation \
        fonts-noto-color-emoji \
        fonts-tlwg-loma-otf \
        fonts-unifont \
        fonts-wqy-zenhei \
        git \
        libasound2 \
        libatk-bridge2.0-0 \
        libatk1.0-0 \
        libatspi2.0-0 \
        libcairo2 \
        libcups2 \
        libdbus-1-3 \
        libdrm2 \
        libfontconfig1 \
        libfreetype6 \
        libgbm1 \
        libglib2.0-0 \
        libnspr4 \
        libnss3 \
        libpango-1.0-0 \
        libx11-6 \
        libxcb1 \
        libxcomposite1 \
        libxdamage1 \
        libxext6 \
        libxfixes3 \
        libxkbcommon0 \
        libxrandr2 \
        pkg-config \
        python3 \
        python3-dev \
        python3-pip \
        python3-venv \
        ripgrep \
        xfonts-scalable \
        xvfb \
    && rm -rf /var/lib/apt/lists/*

# Keep uv off the apt layer so a truncated wheel does not rebuild apt.
RUN n=0 \
    && until python3 -m pip install --break-system-packages --no-cache-dir uv; do \
         n=$((n + 1)); \
         if [ "$n" -ge 3 ]; then exit 1; fi; \
         sleep 5; \
       done

ENV HERMES_HOME=/data \
    API_SERVER_ENABLED=true \
    API_SERVER_HOST=0.0.0.0 \
    API_SERVER_PORT=8642 \
    PYTHONUNBUFFERED=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PATH=/opt/hermes-agent/.venv/bin:$PATH

# Ambil hanya commit rilis yang di-pin. Clone default branch menarik tip main
# yang jauh lebih baru dan sering putus di tengah transfer TLS.
RUN git init /opt/hermes-agent \
    && git -C /opt/hermes-agent config http.version HTTP/1.1 \
    && git -C /opt/hermes-agent remote add origin "${HERMES_REPO}" \
    && n=0 \
    && until GIT_TERMINAL_PROMPT=0 git -C /opt/hermes-agent fetch --depth 1 origin "${HERMES_REF}"; do \
         n=$((n + 1)); \
         if [ "$n" -ge 3 ]; then exit 1; fi; \
         sleep 5; \
       done \
    && git -C /opt/hermes-agent checkout --detach FETCH_HEAD \
    && test "$(git -C /opt/hermes-agent rev-parse HEAD)" = "${HERMES_REF}"

RUN cd /opt/hermes-agent \
    && uv venv .venv --python python3 \
    && uv pip install -e ".[all]" \
    && uv pip install playwright \
    && test "$(.venv/bin/python -c 'import importlib.metadata as m; print(m.version("hermes-agent"))')" = "${HERMES_VERSION}" \
    && python -m playwright install chromium \
    && python -m playwright install-deps --dry-run chromium \
    && rm -rf /var/lib/apt/lists/*

COPY runtime/entrypoint.sh /usr/local/bin/hermes-fleet-runtime-entrypoint
COPY runtime/configure.py /usr/local/bin/hermes-fleet-runtime-configure
RUN chmod +x /usr/local/bin/hermes-fleet-runtime-entrypoint /usr/local/bin/hermes-fleet-runtime-configure \
    && mkdir -p /data /workspace

ENV HERMES_FLEET_RUNTIME_BUILD_ID=${RUNTIME_BUILD_ID}

LABEL io.hermes-fleet.hermes-version="${HERMES_VERSION}" \
      io.hermes-fleet.hermes-ref="${HERMES_REF}" \
      io.hermes-fleet.runtime-config-schema="2" \
      io.hermes-fleet.runtime-build-id="${RUNTIME_BUILD_ID}"

WORKDIR /workspace
VOLUME ["/data", "/workspace"]
EXPOSE 8642 9119
ENTRYPOINT ["hermes-fleet-runtime-entrypoint"]
CMD ["hermes", "gateway"]
