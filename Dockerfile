# syntax=docker/dockerfile:1
#
# 多阶段构建：前端静态导出 → Go 单二进制（内嵌前端）→ 运行镜像。
# 原 Docker Hub 镜像源（autumn27/artex）已失效，本仓库自此支持从纯源码自助构建：
#   docker compose build        # 或 docker build -t artex:local .
# TARGETARCH 由 buildx 自动注入（amd64/arm64），Go 交叉编译无需 QEMU；
# 运行层只装工具，编译全部在构建阶段完成。
# 国内网络可覆盖模块源（默认已对国内友好）：
#   --build-arg GOPROXY=https://goproxy.cn,direct
#   --build-arg NPM_REGISTRY=https://registry.npmmirror.com
# Release 使用 --build-arg ARTEX_BINARY_SOURCE=prebuilt，复用校验过的发布二进制；
# 默认仍从源码构建，无需下载任何预编译的 ARTEX 产物。
ARG ARTEX_BINARY_SOURCE=source

########## 阶段 1：前端静态导出 ##########
FROM --platform=$BUILDPLATFORM node:22-bookworm AS web
ARG NPM_REGISTRY=https://registry.npmmirror.com
WORKDIR /web
# --ignore-scripts：跳过 prepare(husky)——容器里没有 .git，husky 会失败；
# Next.js/Biome 的平台二进制走 optionalDependencies，不依赖生命周期脚本。
COPY web/package.json web/package-lock.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci --ignore-scripts
COPY web/ ./
RUN NEXT_EXPORT=1 npm run build:static

########## 阶段 2：Go 后端（内嵌前端，静态编译）##########
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS gobuild
ARG TARGETOS
ARG TARGETARCH
ARG ARTEX_BUILD_VERSION=dev
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
WORKDIR /src
COPY go.mod go.sum ./
# 本仓库通过 replace 使用本地 norma；下载依赖前必须先提供它的模块声明。
COPY norma/go.mod norma/go.sum ./norma/
RUN go mod download
COPY . .
# 前端静态产物从 web 阶段拷入（.dockerignore 已排除宿主 dist，不会冲突）
COPY --from=web /web/out/ server/webui/dist/
RUN version="${ARTEX_BUILD_VERSION#v}" \
    && CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build \
       -tags embedui -trimpath -ldflags "-s -w -X main.version=${version}" \
       -o /out/artex ./cmd/artex

FROM scratch AS source
COPY --from=gobuild /out/artex /out/artex

########## Release 可选输入：不重新编译已验证的二进制 ##########
FROM scratch AS prebuilt
ARG TARGETARCH
COPY dist/${TARGETARCH}/artex /out/artex

FROM ${ARTEX_BINARY_SOURCE} AS binary

########## 阶段 3：运行镜像（同原版：工具 + Playwright 预装）##########
FROM python:3.12-slim-bookworm AS runtime
ARG NPM_REGISTRY=https://registry.npmmirror.com
# 与下方 MCP/CLI 的精确依赖对齐，避免工具寻找另一版未安装的 Chromium。
ARG PLAYWRIGHT_VERSION=1.64.0-alpha-1790635538000
ARG PLAYWRIGHT_MCP_VERSION=0.0.83
ARG PLAYWRIGHT_CLI_VERSION=0.1.22
ARG PLAYWRIGHT_DOWNLOAD_HOST
# npm 源可覆盖；浏览器默认使用官方 CDN，也可通过 build arg 指定镜像站。
ENV npm_config_registry=$NPM_REGISTRY \
    PLAYWRIGHT_DOWNLOAD_HOST=$PLAYWRIGHT_DOWNLOAD_HOST
# 常用工具：ripgrep / curl / vim，加一批 recon 常备件（按需增删）。
# Node 与前端构建统一使用 22.x，避免继续使用已结束维护的 20.x。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# 预装 Playwright MCP 与 CLI（全局），运行时不再 npx 联网下载。
# @playwright/mcp：browser MCP 直接 `npx @playwright/mcp`（已全局装好，无需 -y/@latest）。
# @playwright/cli：提供 playwright-cli，装完顺带 --help 验证可执行。
# 再装 playwright（提供浏览器管理），装完用 --with-deps 预置 chromium 及其系统依赖，
# 这样容器内 MCP/CLI 首次启动即可用，不再联网下载浏览器。
RUN npm install -g "@playwright/mcp@${PLAYWRIGHT_MCP_VERSION}" \
      "@playwright/cli@${PLAYWRIGHT_CLI_VERSION}" "playwright@${PLAYWRIGHT_VERSION}" \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && node -e 'const fs = require("node:fs"), cp = require("node:child_process"); const root = cp.execFileSync("npm", ["root", "-g"], {encoding:"utf8"}).trim(); fs.symlinkSync(require(root + "/playwright").chromium.executablePath(), "/usr/local/bin/artex-chromium");' \
    && rm -rf /var/lib/apt/lists/*
# MCP/CLI 默认使用容器已安装的 Chromium，无需 Chrome、显示服务器或运行时下载。
# 容器以 root 运行，Chromium 的进程沙箱不可用；可在自定义非 root 部署中覆盖。
ENV PLAYWRIGHT_MCP_EXECUTABLE_PATH=/usr/local/bin/artex-chromium \
    PLAYWRIGHT_MCP_HEADLESS=true \
    PLAYWRIGHT_MCP_SANDBOX=false
WORKDIR /app
# 内置 browser MCP 使用 npx @playwright/mcp。npm exec 不保证解析全局包；
# 将预装依赖暴露到应用的本地依赖路径，子工作目录也可离线启动同一版本。
RUN ln -s "$(npm root -g)" /app/node_modules \
    && npx --offline --no-install @playwright/mcp --help > /dev/null
# 构建阶段产出的单二进制（内嵌前端）
COPY --from=binary /out/artex /app/artex
# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 artex —— docker stop 只把信号发给 PID 1，
# 不转发的话 artex 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
COPY start.sh /app/start.sh
# rclone + offload.sh：Railway 部署的磁盘空间管理，把老归档推到 Google Drive 腾空间
RUN curl -fsSL https://downloads.rclone.org/rclone-current-linux-amd64.zip -o /tmp/rclone.zip \
    && unzip -o /tmp/rclone.zip -d /tmp/rclone-x \
    && cp /tmp/rclone-x/rclone-*/rclone /usr/local/bin/rclone \
    && chmod +x /usr/local/bin/rclone \
    && rm -rf /tmp/rclone.zip /tmp/rclone-x
COPY offload.sh /app/offload.sh
RUN chmod +x /app/artex /app/start.sh /app/offload.sh
COPY skills/ /app/skills/
# data/（SQLite + jwt.key）持久化点 —— Railway 侧通过 volume 挂载，VOLUME 指令会被拒绝，故删除
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
