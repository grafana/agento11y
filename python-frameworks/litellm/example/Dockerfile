FROM ghcr.io/berriai/litellm:v1.95.0@sha256:af806882b7a6ced41658db5b6a7e98ed7b9b51d03b935e0417bf1c8552d688af

COPY python /tmp/agento11y
COPY python-frameworks/litellm /tmp/agento11y-litellm

# The image ships a uv-managed venv at /app/.venv with no pip in it, so pip is
# bootstrapped before the two local packages are installed. Installing from
# PyPI instead needs the same bootstrap:
#   RUN python -m ensurepip --upgrade && \
#       python -m pip install --no-cache-dir agento11y agento11y-litellm
RUN python -m ensurepip --upgrade && \
    python -m pip install --no-cache-dir /tmp/agento11y /tmp/agento11y-litellm && \
    rm -rf /tmp/agento11y /tmp/agento11y-litellm
