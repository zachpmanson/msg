.PHONY: build typecheck clean format

build:
	go build -o msg .

typecheck:
	go vet ./...

clean:
	rm -f msg

format:
	gofmt -w .
