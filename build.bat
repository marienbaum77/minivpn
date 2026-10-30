@echo off
go build -trimpath -ldflags="-H=windowsgui -s -w" -o minivpn.exe ./cmd/minivpn
