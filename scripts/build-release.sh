#!/usr/bin/env bash
# build-release.sh：交叉编译 linux-amd64/arm64 纯静态二进制并打成发布包到 release/。
# 用法：bash scripts/build-release.sh [版本号]（缺省 git describe，无 tag 回退 v0.1.0）
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo v0.1.0)}
LDFLAGS="-s -w -X main.version=${VERSION}"
STAGE_ROOT="release/pkg"
ARCHS=(amd64 arm64)

# 从 git remote 推导 GitHub 拉取基址（注入 install.sh 的 DEFAULT_RELEASE_BASE_URL）。
# 推导失败时回退到固定仓库地址。
DEFAULT_REPO_URL="https://github.com/jayvzh/cf-opt-adguard/releases/latest/download"
release_base_url() {
    local remote
    remote=$(git remote get-url origin 2>/dev/null || true)
    remote=${remote%.git} # bash ERE 无懒惰量词，先剥 .git 后缀再匹配
    if [[ "$remote" =~ github.com[:/](.+)/([^/]+)$ ]]; then
        echo "https://github.com/${BASH_REMATCH[1]}/${BASH_REMATCH[2]}/releases/latest/download"
    else
        echo "$DEFAULT_REPO_URL"
    fi
}
BASE_URL=$(release_base_url)

rm -rf "$STAGE_ROOT"
mkdir -p release

for arch in "${ARCHS[@]}"; do
    stage="$STAGE_ROOT/cf-opt-adguard-${VERSION}-linux-${arch}"
    mkdir -p "$stage"
    echo "==> 构建 linux/${arch} (${VERSION})"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$stage/cf-opt-adguard" ./cmd/cf-opt-adguard
    # 注入拉取基址后拷入安装脚本与配置样例。
    sed "s|^DEFAULT_RELEASE_BASE_URL=.*|DEFAULT_RELEASE_BASE_URL=\"${BASE_URL}\"|" \
        scripts/install.sh > "$stage/install.sh"
    chmod +x "$stage/install.sh" "$stage/cf-opt-adguard"
    cp config.example.yaml "$stage/config.example.yaml"
    tar -czf "release/cf-opt-adguard-${VERSION}-linux-${arch}.tar.gz" -C "$STAGE_ROOT" \
        "cf-opt-adguard-${VERSION}-linux-${arch}"
done

# 校验和 + 产物清单，清理中间 stage 目录。
cd release
rm -rf pkg
sha256sum cf-opt-adguard-"${VERSION}"-linux-*.tar.gz > checksums.txt
echo
echo "==> 发布产物（$(pwd)）:"
ls -lh cf-opt-adguard-"${VERSION}"-linux-*.tar.gz checksums.txt
