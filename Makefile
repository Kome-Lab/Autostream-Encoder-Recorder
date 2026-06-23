.PHONY: test build docker-local-up docker-local-down docker-local-logs

test:
	go test ./...

build:
	go build ./...

docker-local-up:
	docker compose -f docker-compose.yml -f docker-compose.local.yml up --build -d

docker-local-down:
	docker compose -f docker-compose.yml -f docker-compose.local.yml down

docker-local-logs:
	docker compose -f docker-compose.yml -f docker-compose.local.yml logs -f
