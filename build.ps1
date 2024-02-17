set GO111MODULE=on
set CGO_ENABLED=0
set GOAMD64=v3

go build -v -ldflags '-w -s' -trimpath -o build/metatube-server.exe cmd/server/main.go