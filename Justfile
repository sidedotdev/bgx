build:
	go install ./cmd/bgx

release *args:
	./scripts/release.sh {{args}}