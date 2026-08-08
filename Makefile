.PHONY: help run-python run-go run-js build-python build-go build-js build-all up down logs clean clean-logs

# Default target
help:
	@echo "Available commands:"
	@echo "  make run-python     Run logreq Python implementation locally"
	@echo "  make run-go         Run logreq Go implementation locally"
	@echo "  make run-js         Run logreq Node.js implementation locally"
	@echo "  make build-python   Build logreq Python Docker image"
	@echo "  make build-go       Build logreq Go Docker image"
	@echo "  make build-js       Build logreq Node.js Docker image"
	@echo "  make build-all      Build all language Docker images"
	@echo "  make up             Start all services with Docker Compose"
	@echo "  make down           Stop Docker Compose containers"
	@echo "  make logs           View Docker Compose logs"
	@echo "  make clean-logs     Clean log files without needing sudo"
	@echo "  make clean          Clean python cache, log files and temporary test files"

# Local execution
run-python:
	cd src/python && uv run main.py

run-go:
	cd src/go && go run main.go

run-js:
	cd src/js && node main.js

# Docker build targets
build-python:
	docker build -f src/python/Dockerfile.python -t logreq-python:latest src/python

build-go:
	docker build -f src/go/Dockerfile.golang -t logreq-go:latest src/go

build-js:
	docker build -f src/js/Dockerfile.javascript -t logreq-js:latest src/js

build-all: build-python build-go build-js

# Docker Compose targets
up:
	mkdir -p logs
	UID=$$(id -u) GID=$$(id -g) docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

clean-logs:
	find logs -mindepth 1 ! -name '.gitkeep' -exec rm -rf {} +

clean: clean-logs
	find . -type d -name "__pycache__" -exec rm -rf {} +
	find . -type f -name "*.pyc" -delete
