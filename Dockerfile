FROM python:3.12-alpine

# Pin docker-cli to an explicit version so an alpine base-image bump can't
# silently swap the build path out from under us (ADR-2, A-3). Matches the
# version shipped by this base image's alpine release (3.24) at time of
# writing; bump deliberately, not by accident.
# Do NOT add docker-cli-buildx: BuildKit here goes through daemon-side
# `POST /build?version=2`, selected at runtime via DOCKER_BUILDKIT in .env,
# not baked into the image (ADR-2). buildx's docker-container driver needs
# an HTTP Upgrade the socket-proxy's GRPC=0 ACL rejects (design.md EF-2).
RUN apk add --no-cache docker-cli=29.5.3-r1 git

WORKDIR /app
COPY app.py graphify_refresh.py ./
CMD ["python", "app.py"]
