#! /bin/bash
#set -x
NAME="spp"

export GO111MODULE=on

VERSION=${VERSION:-"0.8.0"}
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS="-s -w -X 'github.com/esrrhs/spp/version.Version=${VERSION}' -X 'github.com/esrrhs/spp/version.GitCommit=${GIT_COMMIT}' -X 'github.com/esrrhs/spp/version.BuildTime=${BUILD_TIME}'"

build_list=$(go tool dist list)

rm -rf pack
rm -f pack.zip
mkdir -p pack

go mod tidy

for line in $build_list; do
  os=$(echo "$line" | awk -F"/" '{print $1}')
  arch=$(echo "$line" | awk -F"/" '{print $2}')

  # Skip unsupported or mobile targets
  if [ "$os" == "android" ] || [ "$os" == "ios" ]; then
    continue
  fi
  if [ "$arch" == "riscv64" ]; then
    continue
  fi
  if [ "$arch" == "mips64" ] && [ "$os" == "openbsd" ]; then
    continue
  fi
  if [ "$arch" == "mips64p32" ] || [ "$arch" == "mips64p32le" ]; then
    continue
  fi

  echo "==> Building ${NAME} for OS=${os} ARCH=${arch} (Version=${VERSION}, Commit=${GIT_COMMIT})"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags="${LDFLAGS}" -o "${NAME}"
  if [ $? -ne 0 ]; then
    echo "ERROR: os=${os} arch=${arch} build failed"
    exit 1
  fi

  ARCHIVE_NAME="${NAME}_${os}_${arch}.zip"
  if [ "$os" = "windows" ]; then
    mv "${NAME}" "${NAME}.exe"
    zip "${ARCHIVE_NAME}" "${NAME}.exe"
    rm -f "${NAME}.exe"
  else
    zip "${ARCHIVE_NAME}" "${NAME}"
    rm -f "${NAME}"
  fi

  if [ $? -ne 0 ]; then
    echo "ERROR: os=${os} arch=${arch} packaging failed"
    exit 1
  fi

  mv "${ARCHIVE_NAME}" pack/
  echo "==> Done ${os}/${arch}"
done

zip pack.zip pack/ -r

echo "All builds completed successfully. Artifacts stored in pack/ and pack.zip"
