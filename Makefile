BIN := bin
FAKE_URL := http://localhost:8081/weatherapi/locationforecast/2.0/compact

.PHONY: build run run-fake test lint docker

## build: build the service and the fake met.no into bin/
build:
	go build -o $(BIN)/ ./cmd/...

## run: run one in-memory instance against the real met.no; USER_AGENT must name the application and give contact details
run:
	@test -n "$(USER_AGENT)" || { echo 'USER_AGENT is required by the met.no Terms of Service, e.g. make run USER_AGENT="how-is-the-weather-at-api-dev/0.1 you@example.com"'; exit 2; }
	go run ./cmd/how-is-the-weather-at-api -in-memory -user-agent "$(USER_AGENT)" -upstream-rate 1

## run-fake: run the fake met.no on port 8081 and one in-memory instance of the service on port 8080
run-fake: build
	@$(BIN)/fakemetno -port 8081 & fake=$$!; trap 'kill $$fake' EXIT INT TERM; \
	$(BIN)/how-is-the-weather-at-api -in-memory -upstream-url $(FAKE_URL) -user-agent "how-is-the-weather-at-api-dev local"

## test: run all tests with the race detector
test:
	go test -race ./...

## lint: run golangci-lint
lint:
	golangci-lint run

## docker: build the container image
docker:
	docker build -t how-is-the-weather-at-api .
