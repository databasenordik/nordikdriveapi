.PHONY: test test-unit test-integration

test: test-unit

test-unit:
	go test ./... -count=1 -coverprofile=coverage.unit.out

test-integration:
	go test -tags=integration ./... -count=1 -coverpkg=./... -coverprofile=coverage.integration.out
