FROM node:22-bookworm-slim

ARG HERMES_REPO=https://github.com/NousResearch/hermes-agent.git
ARG HERMES_REF=7acaff5ef2bcbaa22bd23b72efe60906123a4f55
ARG HERMES_VERSION=0.18.2
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
    && python3 -m pip install --break-system-packages --no-cache-dir uv \
    && rm -rf /var/lib/apt/lists/*

ENV HERMES_HOME=/data \
    API_SERVER_ENABLED=true \
    API_SERVER_HOST=0.0.0.0 \
    API_SERVER_PORT=8642 \
    PYTHONUNBUFFERED=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    PATH=/opt/hermes-agent/.venv/bin:$PATH

RUN git clone --depth 1 "${HERMES_REPO}" /opt/hermes-agent \
    && cd /opt/hermes-agent \
    && git fetch --depth 1 origin "${HERMES_REF}" \
    && git checkout "${HERMES_REF}" \
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
