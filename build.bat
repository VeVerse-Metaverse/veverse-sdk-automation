@echo off
go build -o sdk-automation.exe -ldflags "-s -w" main.go
@REM set GOOS=darwin
@REM set GOARCH=amd64
@REM go build -o metaverse-sdk-automation-mac -ldflags "-s -w" main.go
@REM set GOOS=darwin
@REM set GOARCH=arm64
@REM go build -o metaverse-sdk-automation-mac-m1 -ldflags "-s -w" main.go