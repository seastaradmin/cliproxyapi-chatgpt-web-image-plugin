# CPA ChatGPT 网页生图插件

独立插件仓库，支持使用 CPA 已登录的 free Codex OAuth 账号进行文生图和多图参考。模型路由名为 `gpt-image-2-web`。

## 安装

1. 从本仓库的 [Releases](https://github.com/seastaradmin/cliproxyapi-chatgpt-web-image-plugin/releases) 下载对应系统和 CPU 的 `.tar.gz`，并用同一 Release 的 `SHA256SUMS` 校验。
2. 解压后确认机器已安装 Git 和 [uv](https://docs.astral.sh/uv/getting-started/installation/)。安装脚本通过 uv 安装 Python 3.13 和锁定版本的依赖，不需要安装 Go 或克隆 CPA 源码。
3. 在解压目录执行：

```sh
sh install.sh /你的/CPA/plugins "$HOME/.local/share/cpa/chatgpt-web-image-0.3.0"
```

4. 将脚本打印的 `chatgpt-web-image` 配置合并进 CPA 原有的 `plugins.configs`，并开启 `plugins.enabled`。不要覆盖其他插件配置。脚本也会把配置片段写入新运行目录的 `cpa-config-snippet.yaml`。
5. 等待 CPA 重载，确认管理页面中插件版本为 `0.3.0`，且 `/v1/models` 出现 `gpt-image-2-web`。

支持 macOS/Linux，arm64/amd64。需要 CPA 支持 C ABI 1、协议 schema 6、模型路由、静态执行器和账号读取回调；已在 `seastaradmin/CLIProxyAPI` 的 `enterprise-base` 版本验证。不能假设任意旧版 CPA 都兼容。

发布包包含动态库、Python 工作进程和安装脚本。CPA 商店仅安装动态库时不会自动准备 Python 运行目录，因此请按上述流程安装完整包。

## 使用

文生图：`POST /v1/images/generations`，请求示例：

```json
{"model":"gpt-image-2-web","prompt":"一只橘猫坐在窗边","n":1}
```

多图参考：

```sh
curl http://127.0.0.1:8889/v1/images/edits \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -F 'model=gpt-image-2-web' \
  -F 'prompt=将图一的主体放在左侧，图二的主体放在右侧，保留各自颜色和形状' \
  -F 'image[]=@reference-1.png' \
  -F 'image[]=@reference-2.png' \
  -F 'n=1'
```

也支持 JSON `images` 数组中的 base64/data URL。最多 10 张参考图，每张不超过 10 MiB；原始请求和转为 JSON 后的请求都不能超过 32 MiB。`n=1` 是输出一张图，不是只能输入一张参考图。暂不支持流式、蒙版或远程图片 URL。

## 模型与尺寸

`gpt-image-2-web` 是插件别名，网页底层实际使用的模型版本无法从现有响应独立确认，不保证是 Image 2、2.5 或 2.6。`size` 和 `quality` 作为提示传给网页，不能强制指定最终像素尺寸。

实测：请求 `2048x2048`、`high` 返回 `1254x1254`；两图参考的横图返回 `1774x887`。插件没有对图片缩放或放大，不能保证原生 2K/4K。

## 升级与源码构建

每个版本使用新的动态库文件名和运行目录，再更新配置中的 `store.version` 和 `runtime_dir`。不要直接覆盖已加载的动态库。删除配置前可用 `enabled: false` 停用插件。

源码仓库可独立编译，Go 1.26+ 和 C 编译器即可，无本地 CPA 路径依赖：

```sh
go test -race ./...
sh scripts/package.sh
```

网页协议复用 [chatgpt2api](https://github.com/basketikun/chatgpt2api) 的固定提交 `95c8024f2884a620f9b36d2d2cb504b590ce82d9`，由安装脚本下载。账号凭据只经进程内存/stdin 传递，不另存账号池；参考图上传到所选 ChatGPT 账号，生成会话会保留。网页额度和可用性取决于账号。
