.PHONY: help run-python build-python up down logs clean

# Default target
help:
	@echo "Available commands:"
	@echo "  make run-python     Run logreq Python service locally"
	@echo "  make build-python   Build logreq Python Docker image"
	@echo "  make up             Start logreq with Docker Compose"
	@echo "  make down           Stop Docker Compose containers"
	@echo "  make logs           View Docker Compose logs"
	@echo "  make clean          Clean python cache and temporary test logs"

# Run Python service locally
run-python:
	cd src/python && uv run main.py

# Build Python Docker image
build-python:
	docker build -f src/python/Dockerfile.python -t logreq-python:latest src/python

# Docker Compose targets
up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

clean:
	find . -type d -name "__pycache__" -exec rm -rf {} +
	find . -type f -name "*.pyc" -delete
