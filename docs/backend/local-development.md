---
title: 本地开发
description: 前后端分开启动时的本地开发方式
---

# 本地开发

如果你需要改代码，建议前后端分开启动

## 1. 准备环境变量

```bash
cp .env.example .env
```

默认配置下：

- 后端端口是 `8080`
- 前端端口是 `3000`
- SQLite 数据库是 `data/infinite-canvas.db`

## 2. 启动后端

在仓库根目录执行：

```bash
go run .
```

后端会读取根目录 `.env`，并监听：

```text
http://127.0.0.1:8080
```

## 3. 启动前端

在 `web` 目录执行：

```bash
bun run dev
```

前端默认访问：

```text
http://localhost:3000
```

开发代理默认转发到 `http://127.0.0.1:8080`。如果你的后端端口不同，启动前设置 `API_BASE_URL`

## 常见场景

- 改画布、页面和交互：主要看 `web/`
- 改接口、业务逻辑和数据库：主要看仓库根目录下的 Go 代码
- 改文档站内容：主要看 `docs/content/docs/`

## Go 后端 Codex 渠道验证

先安装 Codex CLI（实现按 `0.161.0` 的 app-server 协议接入），以运行 Go 后端的用户登录：

```bash
codex --version
codex login
codex login status
```

在根目录 `.env` 中设置后重启 Go 后端：

```dotenv
CODEX_ENABLED=true
CODEX_COMMAND=codex
```

如果后端找不到命令，`CODEX_COMMAND` 填 CLI 的绝对路径，例如本机 Homebrew 的 `/opt/homebrew/bin/codex`。本地不设置 `CODEX_HOME` 时沿用当前用户的默认 Codex 目录；指定独立目录时，先用相同 `CODEX_HOME` 执行登录。`CODEX_WORK_DIR` 默认 `data/codex-tasks`，每次请求的临时目录会在结束后删除。

进入管理后台的系统设置，新增渠道，协议选择「Codex App Server」，保留 `codex-text` 和 `codex-image`。可选的「Codex 执行模型」填写拉取列表中的实际模型 ID；留空使用 Codex 默认模型。这两个名称是本项目的功能入口，不是 Codex 官方模型 ID。保存并启用渠道，为测试账号开放系统渠道权限；前端模型配置切换为「云端渠道」后选择该渠道。

依次验证：拉取模型、后台测试 `codex-text`、画布文本节点流式回复、带参考图片的理解、`codex-image` 文生图和参考图编辑、生成中停止、后端重启后任务失败及重新生成。图片测试需要账号支持原生生图；后台对图片模型的「测试」只检查登录和连通，实际生图在画布验证。尺寸、质量、张数和蒙版以原生工具能力为准，不保证 API 级精确参数控制。

Go 不设置 Codex 请求并发上限。文本和生图请求分别创建独立线程，同时提交到共享 app-server；每个请求有独立工作目录和渠道超时。验证时同时提交多个生图任务，并在生成期间发送文本请求、停止其中一个图片任务，检查其余任务是否继续执行。实际完成速度和上游限流以 Codex 服务返回为准。

Go 通过 stdin/stdout JSON-RPC 管理 app-server，无需手动启动 app-server，也不需要 Node SDK 服务或 app-server 网络端口。浏览器只请求本项目后端；模型推理仍由 Codex 服务完成，需要网络连接，并非离线本机推理。

Docker 镜像包含 Codex CLI。保持现有 `./data:/app/data` 挂载，设置 `.env` 的 `CODEX_ENABLED=true`，容器启动后执行：

```bash
docker compose exec app codex login --device-auth
docker compose exec app codex login status
```

Docker 中 `CODEX_HOME` 默认 `/app/data/codex-home`，持久化登录状态。镜像构建和容器真实生成仍需验证。
