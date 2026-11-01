FROM python:3.13.3-slim-bookworm@sha256:56a11364ffe0fee3bd60af6d6d5209eba8a99c2c16dc4c7c5861dc06261503cc
ARG HUB_UID=1000
ARG HUB_GID=1000
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends git openssh-client ca-certificates && rm -rf /var/lib/apt/lists/*
RUN groupadd --gid ${HUB_GID} hub && useradd --uid ${HUB_UID} --gid ${HUB_GID} --create-home --shell /usr/sbin/nologin hub
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && pip check
COPY hub ./hub
COPY tests ./tests
CMD ["python", "-m", "uvicorn", "hub.app:app", "--host", "127.0.0.1", "--port", "8088", "--no-access-log"]
