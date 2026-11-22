# Harbor Mail 第三方许可声明

Harbor Mail 自有代码采用 [Apache-2.0](LICENSE)。以下第三方组件保留各自的版权声明、许可和归属要求。

## 二进制与网页资源

- [Go 及 Go 模块](docs/licenses/go-runtime.txt)列出 Linux 和 Windows 的 amd64、arm64 构建所用的模块版本和上游声明。MySQL 驱动中的 MPL-2.0 文件可从 [v1.10.2 对应源码](https://proxy.golang.org/github.com/go-sql-driver/mysql/@v/v1.10.2.zip)获取。Harbor Mail 按 `go.mod` 和 `go.sum` 使用上游代码。
- [Tailwind CSS](docs/licenses/tailwindcss.txt)采用 MIT。发行物包含 Tailwind 生成的网页样式。
- [Remix Icon](web/Remix-Icon-License.txt)和 [Kubai Icons](web/Kubai-Icons-License.txt)分别按 Remix Icon License v1.0 和 MIT 提供。图标集用于网页控件。
- [Harbor Han SC](web/fonts/source-han-OFL.txt)是 Adobe 思源黑体的重命名版本。[Noto Sans SC](web/notosanssc-v41/OFL.txt)用于回退。两者采用 SIL OFL 1.1。

`harbormail-licenses.tar.gz` 与 GitHub Release 的可执行文件一同提供，保留上述路径。官方 Harbor Mail 镜像在 `/usr/share/licenses/harbormail/` 提供同一套声明。

## Docker 镜像与独立服务

官方镜像中的 Alpine 运行组件继续按各自条款分发。[组件清单](docs/licenses/alpine-runtime.tsv)依次列出架构、包名、版本、许可证标识、Alpine 源码包名及 aports 提交。对应的源码包、补丁、配置和构建文件可从 [Alpine 对应源码归档](https://github.com/makessr/HarborMail/releases/download/v0.2.18/harbormail-alpine-sources.tar.gz) 获取。

Compose 中的 MySQL、Redis、Rspamd 和 ClamAV 由各自的上游镜像提供，适用对应镜像与版本的条款。

## 维护

更新 Go 版本或模块后，在仓库根目录运行 `node .github/scripts/update-go-notices.mjs`，核对新增条款并提交生成的清单。更新 Alpine 基础镜像或 APK 包时一起更新 `alpine-runtime.tsv`、对应源码归档与发行工作流中的归档来源。
