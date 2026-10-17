.PHONY: all agent client clean

all: agent client

agent:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/devtools-rdp-agent.exe ./cmd/devtools-rdp-agent

client:
	go build -trimpath -o dist/devtools-rdp ./cmd/devtools-rdp

clean:
	rm -rf dist/devtools-rdp dist/devtools-rdp-agent.exe
