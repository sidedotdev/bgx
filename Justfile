build:
	go build ./cmd/bgx

install:
	go install ./cmd/bgx

release *args:
	./scripts/release.sh {{args}}