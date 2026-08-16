FROM python:3.12-alpine
RUN apk add --no-cache docker-cli git
WORKDIR /app
COPY app.py .
CMD ["python", "app.py"]
