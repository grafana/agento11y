FROM ghcr.io/berriai/litellm:v1.82.3-stable.patch.2

COPY sdks/python /tmp/agento11y
COPY sdks/python-frameworks/litellm /tmp/agento11y-litellm

RUN pip install --no-cache-dir /tmp/agento11y /tmp/agento11y-litellm && \
    rm -rf /tmp/agento11y /tmp/agento11y-litellm
