#!/usr/bin/env bash
set -euo pipefail
tools_dir=$(mktemp -d "${RUNNER_TEMP:-/tmp}/nordik-contract.XXXXXXXX")
trap 'rm -rf -- "$tools_dir"' EXIT
mkdir "$tools_dir/expected"
cp internal/identitypb/*.go "$tools_dir/expected/"
curl --fail --silent --show-error --location \
  https://github.com/protocolbuffers/protobuf/releases/download/v33.4/protoc-33.4-linux-x86_64.zip \
  --output "$tools_dir/protoc.zip"
echo "c0040ea9aef08fdeb2c74ca609b18d5fdbfc44ea0042fcfbfb38860d35f7dd66  $tools_dir/protoc.zip" | sha256sum -c
unzip -q "$tools_dir/protoc.zip" -d "$tools_dir/protoc"
go build -o "$tools_dir/protoc-gen-go" google.golang.org/protobuf/cmd/protoc-gen-go
GOBIN="$tools_dir" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
"$tools_dir/protoc/bin/protoc" --proto_path=proto \
  --plugin="protoc-gen-go=$tools_dir/protoc-gen-go" \
  --plugin="protoc-gen-go-grpc=$tools_dir/protoc-gen-go-grpc" \
  --go_out=. --go_opt=module=nordik-drive-api \
  --go-grpc_out=. --go-grpc_opt=module=nordik-drive-api \
  proto/identity/v1/identity.proto
diff -ru "$tools_dir/expected" internal/identitypb
