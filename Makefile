BINARY := SopsSecretGenerator

export GO111MODULE=on

$(BINARY): main.go
	go build -o $@ $<

.PHONY: test
test:
	go test -v -race

.PHONY: test-coverage
test-coverage:
	go test -v -race -coverprofile=coverage.txt -covermode=atomic

.PHONY: clean
clean:
	-rm -rf $(BINARY)
