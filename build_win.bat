@echo off

rem  COMMID=$(git describe --always --dirty)  v1.250608.0-8-ga802c5ed

go build -o build_assets/xray.exe -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -v ./main

go build -o build_assets/xray.exe -ldflags="-s -w -buildid=" -v ./main

go build -o build_assets/xray.exe -v ./main

rem macos  go build -o build_assets/xray -trimpath -buildvcs=false -ldflags="-X github.com/xtls/xray-core/core.build=v1.250608.0-8-ga802c5ed -s -w -buildid=" -v ./main
