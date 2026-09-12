# Telegram 私聊归档与 AI 秘书

通过 Telegram「聊天自动化」连接账号，把双向私聊保存到**你与 Bot 私聊中的联系人话题**，并按需开启 AI 自动回复。无需归档群、Webhook、域名、公网端口或 Cloudflare。仅配置的 OWNER_ID 可以接入和管理。

当前版本已全部改为 **Go + 静态链接 SQLite**。最终镜像使用 `scratch`，只有单个可执行文件和 HTTPS CA 证书，没有 Node.js、npm、Shell 或发行版运行环境。支持 Linux amd64 / arm64；状态集中保存在 `/data/bot.sqlite`。

HTTP、JSON、长轮询、调度均使用 Go 标准库，唯一第三方依赖是编译进二进制的 SQLite 驱动。默认 `GOMEMLIMIT=32MiB` 是 Go 管理内存的软目标，**不是容器总内存上限**，不涵盖 SQLite C 内存、网络缓冲等。模型并发最多 4 个，不常驻每位联系人的数据库连接。

## Linux Docker 部署

### Telegram 准备

1. 在官方 @BotFather 用 `/newbot` 创建 Bot，保存 Token。
2. 开启 **Secretary Mode / Business Mode** 和 **Topics in Private Chats / 私聊话题模式**。菜单名称可能随客户端版本变化；API `getMe` 的 `can_connect_to_business` 和 `has_topics_enabled` 必须为 true，启动会检查。
3. 用主人账号打开 Bot 私聊，发送 `/start`，否则 Bot 无法主动发消息和注册管理命令。
4. 准备主人的数字 Telegram 用户 ID，填写 `OWNER_ID`，不是用户名、电话号码或 Bot ID。
5. 在 Telegram 设置的「聊天自动化 / Chat Automation」（部分客户端位于 Business 设置）连接 Bot，选择需要归档的私聊范围并授权回复。该入口的账号资格以 Telegram 实际开放条件为准。

官方资料：[Secretary Mode](https://core.telegram.org/bots/features#business-bots)、[私聊话题](https://core.telegram.org/bots/api#createforumtopic)。

### 下载配置与启动

先安装 [Docker Engine 和 Compose 插件](https://docs.docker.com/engine/install/)，然后运行：

```bash
mkdir -p tg-saventalk-bot && cd tg-saventalk-bot
curl -fsSLO https://raw.githubusercontent.com/krabdo/tg-saventalk-bot/main/compose.yaml
curl -fsSL https://raw.githubusercontent.com/krabdo/tg-saventalk-bot/main/.env.example -o .env
chmod 600 .env
nano .env
docker compose pull
docker compose up -d
docker compose logs --tail=50
```

填写 `.env`：

```dotenv
BOT_TOKEN=123456:你的BotToken
OWNER_ID=你的数字用户ID
AI_BASE_URL=
AI_MODEL=
AI_API_KEY=
```

AI 三项可留空，仅归档也能运行。需要 AI 时填写服务商的 HTTPS base URL、模型名、API Key；base URL 若包含 `/v1` 应保留。程序追加 `/chat/completions`，不要填写完整 completions URL。没有预设端点或模型。

修改配置后执行 `docker compose up -d --force-recreate`。AI 初始全局暂停，在 Bot 私聊执行 `/resume all` 开启；以后重启保留状态。

**启动自动移除旧 Webhook（不丢弃待处理更新）、设置命令菜单并开始长轮询，无需手动注册 Webhook。** 从 Workers 迁移前先停用旧 Worker，避免后台任务继续代回复。同一 Token 只能运行一个服务。详见 [Telegram 长轮询说明](https://core.telegram.org/bots/faq)。

镜像为 `ghcr.io/krabdo/tg-saventalk-bot:latest`。如果管理员尚未公开 Package，匿名拉取会失败：GitHub 个人主页 Packages → tg-saventalk-bot → Package settings → Change visibility 设置 Public，或用 `read:packages` 凭据登录 GHCR。不要把 GitHub Token 写进 Bot 的 `.env`。

通过 `docker compose ps` 查看健康状态、`docker compose logs --tail=100 bot` 查看日志，然后在 Bot 私聊执行 `/status all`。用第二个账号给主人发消息，确认自动创建联系人话题。

## 功能

- 一个联系人 ID 固定映射一个 Bot 私聊话题。保存对方、主人和 AI 消息，标注发送者、时间和原消息 ID。
- 文本保留格式；图片、视频、文件、语音、音频、动画、贴纸、视频便笺使用 `file_id` 重发。相册逐项归档并标记相册 ID。
- 编辑追加版本；删除追加标记，保留原副本。未知原文显示“未收到原消息”。不支持或无法重发的媒体显示“媒体未备份”。
- 不使用用户账号登录、MTProto 或云盘，不补抓历史。只处理 Telegram 投递的 Business 更新；长时间停机可能错过 Telegram 已过期的更新（通常最多保留 24 小时）。Telegram 媒体副本不是独立云盘备份。
- Bot 话题里的普通留言只作备注，不代发给联系人。

## AI 和管理命令

AI **只处理普通文字 `text`**，媒体和媒体说明都不触发回复、也不进入上下文。连续文字等待 3 秒合并，最长 10 秒；每轮最多发一条，超长文本截断并标注。上下文取最近 20 条有效文本，最多 24,000 字符，包含主人和 AI 回复；旧编辑版本和已删除内容仅留在归档。

默认每位联系人累计最多 10 条成功 AI 回复，手动清零后重新计算。发送结果不确定时保留额度占用，避免超额或重复发送。主人在原私聊回复后，该联系人自动暂停；AI 自己的消息只归档。

| 命令 | 功能 |
|---|---|
| `/start`、`/help` | 使用说明 |
| `/status all`、`/status 用户ID` | 连接、AI、额度、归档异常 |
| `/pause all`、`/pause 用户ID` | 全局或联系人暂停 |
| `/resume all`、`/resume 用户ID` | 全局或联系人恢复 |
| `/limit N all`、`/limit N 用户ID` | 默认或专属额度，0 禁止回复 |
| `/reset 用户ID` | 清零额度，不解除暂停 |
| `/prompt 文本` | 全局提示词，也可回复文字执行 `/prompt` |
| `/prompt_show`、`/prompt_reset` | 查看或恢复默认提示词 |
| `/retry` | 联系人话题内重试明确失败的归档 |

联系人话题内可省略目标，例如 `/pause`、`/limit 5`、`/reset`；其他位置必须指定目标。全局暂停优先，恢复全局不解除联系人暂停；修改额度和恢复均不清零。提示词变更使旧生成结果失效。其他用户、群组消息、匿名管理员均不能管理 Bot。

代回复要求连接有效、`can_reply` 权限和最近 24 小时内收到该联系人消息。发送前重新检查权限、暂停、额度与上下文版本；已提交 Telegram 的消息无法被随后到达的暂停撤回。默认提示词要求以助理身份简洁回答、不冒充主人、不虚构、不擅自承诺。

## 故障与恢复

归档和 AI 独立调度，慢模型不会阻塞归档。SQLite 持久化任务、消息、offset；重启恢复待处理任务。429 遵守 `retry_after`，明确失败有限退避；发送结果不确定不自动重发。异常提醒最多每 10 分钟一次，日志不输出正文、提示词或密钥。

| 情况 | 操作 |
|---|---|
| 启动失败 | 检查 Token、OWNER_ID、BotFather 两种模式、是否已 `/start`、是否有其他实例占用 Token 或卷 |
| 无归档 | `/status all` 检查连接，并确认自动化包含该联系人 |
| AI 不回复 | 检查全局/联系人暂停、额度、端点配置、权限和 24 小时窗口 |
| 明确归档失败 | 修复问题后在联系人话题 `/retry` |
| 话题被删除 | 下次发送检测到不存在后创建替代话题并记录断点 |
| 私聊话题关闭 | 在客户端恢复可写再 `/retry`；`reopenForumTopic` 仅支持超级群 |
| 话题创建结果不确定 | 核对正确话题，在该话题执行 `/topic_bind 用户ID` |
| 归档发送结果不确定 | `/status` 找任务 ID；核对已发用 `/archive_resolve 任务ID sent 归档消息ID`，确认未发才用 `/archive_resolve 任务ID retry` |
| AI 发送结果不确定 | 核对原私聊后 `/reset`（清零全部计数并解除不确定占用），按需 `/resume` |

恢复命令在对应联系人话题中执行。Telegram 发送没有幂等键，强制 retry 前需核对，避免重复副本。

## 更新和数据备份

```bash
docker compose pull
docker compose up -d
```

可固定 `sha-...` 标签或镜像 digest。保留同一数据卷；不要执行 `docker compose down -v`，除非要删除所有本地数据。备份先停容器，复制全部 `/data`（包括 SQLite WAL）：

```bash
docker compose stop
docker compose cp bot:/data ./backup-data
docker compose start
```

数据库含私人聊天，未自动加密，历史记录不自动清理，需保护备份并留意磁盘容量。Go 版使用单个 SQLite 数据库和有界连接，上下文按需读取。仅支持单实例、本地磁盘卷，不支持多副本或 NFS 共享卷。新 Bot 或 OWNER_ID 使用新卷。

Workers Durable Objects 数据不自动迁移；新服务从收到的新更新开始归档，旧 Telegram 副本仍保留。

## 从 Node/TypeScript 版升级

先备份数据并停用旧容器，再拉取新镜像，继续挂载同一个数据卷：

```bash
docker compose stop
docker compose cp bot:/data ./backup-before-go
docker compose pull
docker compose up -d --force-recreate
```

首次启动会在单个事务中导入旧版 `account-*.sqlite`、`chat-*.sqlite` 的提示词、暂停、额度、话题、消息版本、归档任务和长轮询 offset，保留联系人话题中的归档任务 ID。事务失败不会留下部分导入结果，下次可重试。

原文件保持不变，Go 版只更新 `bot.sqlite`。中断时已进入发送阶段的任务标记为待核对；旧版尚在生成或等待重试的 AI 结果作废，等待新文字。

不要同时运行两个版本。升级后旧数据库已过期，回退必须恢复升级前的完整卷备份，并核对升级后已经发送的消息，不能直接用旧镜像继续读取旧文件。旧镜像固定标签为 `sha-d1f865f`，仅供必要时回退。

## 开发和 GitHub Packages 发布

```bash
go mod download
go vet ./...
go test -race ./...
go build -trimpath -tags "netgo osusergo sqlite_omit_load_extension" -ldflags "-s -w" -o bot .
# 将 BOT_TOKEN、OWNER_ID 等导出为环境变量后运行
./bot
# Docker 内自动使用 musl 静态链接
docker build -t tg-saventalk-bot .
```

开发需要 Go 1.26.5+ 和 C 编译器。Linux 镜像使用 Alpine/musl 静态编译，移除调试符号，不使用 UPX；最终运行镜像不需要构建工具。以 UID/GID 1000 非 root 运行，不映射端口；支持 `/bot healthcheck` 和离线 `/bot selftest`，无需 Shell。

GitHub Actions 在 push main、`v*` 标签或手动触发时，在原生 amd64、arm64 Linux runner 分别执行 `go vet`、竞态检测、静态镜像构建和非 root 容器测试。两种架构均成功后才发布合并标签。使用仓库 `GITHUB_TOKEN` 的 `packages:write` 权限，无需个人发布密钥。PR 只验证不上传。参见 [GitHub 发布镜像](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images)、[Go SQLite 静态链接](https://github.com/mattn/go-sqlite3#cross-compile)。

自动测试使用真实 SQLite 和模拟 Telegram/AI 接口，覆盖授权、双向归档、编辑删除乱序、额度暂停、模型延迟、429、结果不确定及重启恢复。真实部署后用两个账号验收：发送/编辑/删除文本与媒体，确认副本；设置额度 2 验证停止；主人手动回复验证接管；重启验证映射和额度保留。

业务逻辑源自 tg-saventalk，目前为独立 Go 实现，遵循仓库 GPL-3.0 许可证。旧 TypeScript 源码只保留在 Git 历史中。
