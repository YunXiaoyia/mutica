# Multica × ARIS 自动论文生产线

**你只和一位"主理人"聊天，它调度一支 AI 研究团队，替你把论文从选题写到终稿。**

基于开源智能体协作平台 [Multica](https://github.com/multica-ai/multica) 二次开发的研究自动化系统：
在聊天里对主理人 **Aris** 说一句研究目标，它会实例化一条 8 阶段流水线，调度 8 个专职智能体角色
（选题、文献、实验设计、实验、分析、写作、内审）自动接力推进；阶段完成自动汇报回聊天，
关键节点请你拍板。任务看板全程可见，聊天是唯一交互入口。

> 上游 Multica 的介绍见 [README.zh.md](README.zh.md)；本仓库在其之上增加了依赖门控、
> 流水线模板、聊天编排桥接与 ARIS skill 体系（设计全案见
> [docs/aris-paper-pipeline.md](docs/aris-paper-pipeline.md)）。

---

## 它是怎么工作的

```
你 ──聊天──▶ Aris（主理人）
              │ 实例化 paper-default 流水线
              ▼
  Stage 1 选题 ─▶ Stage 2 文献 ─▶ Stage 3 实验设计 ─▶ Stage 4 实验
        └───── 依赖门控：上一阶段到终态，下一阶段才放行 ─────┘
              ▼
  Stage 5 分析 ─▶ Stage 6 写作 ─▶ Stage 7 内审 ─▶ Stage 8 终稿
```

- **凡推进必有服务端机制**：阶段之间以 `blocked_by` 依赖边串联，完成即自动放行；
  长任务（如训练）走 submit-and-poll，提交集群后立即让出运行时，由自动化每 15 分钟轮询。
- **凡人工必有落点**：3 个人工审核门（选题后、文献后、终稿后）由 Aris 在聊天里给出结论
  和选项，你拍板它才继续；其余阶段全自动。
- **假完成有兜底**：每个阶段任务结束都会推送带 issue 状态的报告进主理人聊天——模型
  提前结束、issue 没到终态时，Aris 能识别并自动重跑，不会悄悄卡死。
- **对抗式质量**：角色绑定异构 CLI（Claude Code / Codex / Antigravity …），执行者与
  审稿者来自不同厂商，避免同族模型的自我偏好。
- **skill 即装备**：86 个 ARIS skills 上架为平台 skill 库，按角色装配，认领任务时物化
  到任务目录；不绑定的 skill 是替补席，随时按需加装。

## 快速开始

### 方案一：Ubuntu / Linux（推荐）

#### 0. 前置要求

Docker（保持运行）、Go 1.21+（首次构建自动下载 1.26 toolchain）、Node 22+ 与 pnpm、Python 3（仅 ARIS 配置脚本需要）：

```bash
npm i -g pnpm        # 或者 corepack enable
```

#### 1. 启动平台

```bash
git clone https://github.com/YunXiaoyia/mutica.git
cd mutica
cp .env.example .env          # 本地开发用默认值即可，不需要改任何内容
make up C=api,web,daemon      # 起 API、前端和本地 daemon
make status                   # 查看本 checkout 分到的地址（每台机器端口不同，以它打印的为准）
```

首次启动会自动完成 pnpm install、拉起 PostgreSQL 容器、执行数据库迁移，需要几分钟。
浏览器打开 `make status` 显示的 **web** 地址注册账号；dev 模式不发真实邮件，**验证码打印在 api 日志里**（`make status` 会显示 logs 路径，形如 `~/.multica/dev/envs/<环境名>/logs/api.log`，搜 `verification code`）。

> **💡 流畅度提示**：Next.js 默认在开发模式下会即时编译路由，首次点击新页面会有数秒卡顿。推荐先执行一次 `pnpm --filter @multica/web build` 编译为生产模式，`make up` 将自动以生产模式运行，全站秒开（详见后文[运行模式](#前端运行模式生产模式秒开-vs-开发模式调试)）。

#### 2. 接入智能体运行时

在本机安装至少一个受支持的 agent CLI（Claude Code、Codex、Antigravity 等）。daemon 由 `make up` 拉起，会自动探测本机 CLI；到网页 **运行时** 页确认在线，并把运行时的**可见性设为公开**（私有运行时只允许归属者本人绑定智能体）。注意：新装的 CLI 最多要等 30 分钟才会被探测到（登录 shell 解析有 30 分钟缓存），想立即生效就 `make daemon`（重启前确认没有正在执行的任务）。daemon 日志在 `~/.multica/daemon.log`。

#### 3. 一键配置 ARIS 花名册

```bash
# 1. 确保步骤 2 中网页「运行时」显示在线且设为公开
# 2. 网页「设置 → API Tokens」创建个人访问令牌（mul_ 开头）
# 3. 网页「设置 → 工作区」复制工作区 UUID（或 GET /api/workspaces 获取）
export MULTICA_BASE_URL=http://localhost:<api端口>
export MULTICA_PAT=mul_xxx
export MULTICA_WORKSPACE_ID=<工作区 uuid>
python3 scripts/aris/bootstrap.py
```

脚本会（幂等，可重复执行）：上架并绑定 86 个 ARIS skills、创建 8 个角色智能体（Aris/Scout/Researcher/Planner/Experimenter/Analyst/Writer/Reviewer）、创建 `paper-default` 流水线模板、打开主理人聊天会话并打印 `orchestrator_session_id`。

---

### 方案二：Windows

在 Windows 上本地部署推荐以下两种途径：

#### 途径 A：WSL 2（Ubuntu）（最推荐，体验最丝滑）

Windows 10/11 建议直接在 WSL 2（Ubuntu）中运行，环境与 Linux 完全一致：
1. 打开 WSL 2 终端，安装 Docker、Go、Node 22+、pnpm、Python 3；
2. 按照上面的 **Ubuntu / Linux** 步骤 1~3 执行即可；
3. WSL 2 与 Windows 端口自动打通，在 Windows 宿主机浏览器直接打开 `http://localhost:<web端口>` 即可访问。

#### 途径 B：PowerShell 原生部署（配合 Docker Desktop）

如果你更倾向于在 Windows PowerShell 宿主机环境中运行：

**0. 前置准备**
- 安装并启动 [Docker Desktop for Windows](https://docs.docker.com/desktop/install/windows-install/)（需开启 WSL 2 引擎）；
- 安装 Node.js 22+、pnpm、Go 1.21+、Python 3，并将其加入系统环境变量 PATH。

**1. 启动基础服务与数据库**
在 PowerShell 中执行：
```powershell
git clone https://github.com/YunXiaoyia/mutica.git
cd mutica
Copy-Item .env.example .env

# 拉起共享 PostgreSQL 17 容器
docker compose up -d postgres

# 执行数据库迁移
cd server; go run ./cmd/migrate up; cd ..

# 安装前端依赖
pnpm install
```

**2. 启动服务与 Daemon**
在不同的 PowerShell 窗口中分别启动：
- **终端 1（后端 API）**：
  ```powershell
  cd server; go run ./cmd/server
  ```
  默认监听 `http://localhost:8080`。
- **终端 2（Web 前端）**：
  - **生产模式（推荐，秒开）**：先运行 `pnpm --filter @multica/web build` 预编译，然后运行 `pnpm --filter @multica/web start`
  - **开发模式（热重载）**：`pnpm dev:web`
  默认监听 `http://localhost:3000`。
- **终端 3（Agent 守护进程）**：
  ```powershell
  cd server; go run ./cmd/multica daemon start
  ```

浏览器访问 `http://localhost:3000` 注册账号（验证码打印在后端控制台终端 1）。在网页「运行时」确认本机在线并将可见性设为公开。

**3. 一键配置 ARIS 花名册**
在 PowerShell 中配置环境变量并运行引导脚本：
```powershell
# 网页「设置 → API Tokens」创建令牌；「设置 → 工作区」查看工作区 UUID
$env:MULTICA_BASE_URL="http://localhost:8080"
$env:MULTICA_PAT="mul_xxx"
$env:MULTICA_WORKSPACE_ID="<工作区 uuid>"
python scripts/aris/bootstrap.py
```

> **提示**：如果使用一键容器化生产自托管，也可直接在 PowerShell 运行官方自建命令：
> `$env:MULTICA_MODE="with-server"; irm https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.ps1 | iex`

---

### 前端运行模式：生产模式（秒开）vs 开发模式（调试）

Next.js 默认在开发模式下采用即时编译（JIT），首次访问每个新页面（设置、智能体、任务看板等）都会现场编译，产生 3~5 秒的卡顿转圈。**日常使用强烈推荐开启生产模式，所有路由全量预编译，全站交互瞬间响应（<50ms）**。

| 模式 | 适用场景 | 优势 | 劣势 | 启动方式 (Linux) | 启动方式 (Windows) |
| --- | --- | --- | --- | --- | --- |
| **生产模式**（推荐） | 日常使用、论文写作、演示 | 全站秒开、交互丝滑、无需等待编译 | 修改前端代码需重新 `build` | 先 `pnpm --filter @multica/web build`<br>后 `make up` 自动以生产模式启动 | `pnpm --filter @multica/web build`<br>`pnpm --filter @multica/web start` |
| **开发模式** | 修改前端 UI / 组件代码 | 支持 HMR 热更新，改动立即生效 | 页面首次加载需现场编译，有卡顿感 | `MULTICA_WEB_DEV=1 make up C=api,web,daemon` | `pnpm dev:web` |

#### Linux / Ubuntu 操作指南

- **开启生产模式（日常首选）**：
  ```bash
  # 1. 预编译 Web 前端（构建产物存放在 apps/web/.next/，仅首次或拉取新代码后执行一次）
  pnpm --filter @multica/web build

  # 2. 正常拉起服务（Makefile 会自动检测构建产物并以生产模式启动，全站秒开）
  make up C=api,web,daemon
  ```
- **切回开发模式（需改前端代码时）**：
  ```bash
  MULTICA_WEB_DEV=1 make up C=api,web,daemon
  # 或者删除构建缓存彻底恢复开发模式：rm -rf apps/web/.next
  ```

#### Windows 操作指南 (PowerShell)

- **开启生产模式**：
  ```powershell
  pnpm --filter @multica/web build
  pnpm --filter @multica/web start
  ```
- **切回开发模式**：
  ```powershell
  pnpm dev:web
  ```

---

### 开始写论文

网页 → **聊天** → **Aris**，一句话描述研究目标。建议带上：
已有代码/数据路径、目标会议或期刊、算力约束、是否有真机。

Aris 会先和你确认约束，然后实例化流水线、Stage 1 自动开跑。之后你只需要：

- 在聊天里收进展汇报；
- 在人工门拍板（它会给出选项）；
- 随时插话改需求、追问细节。

## 日常操作

| 想做什么 | 去哪里 |
| --- | --- |
| 启动 / 停止 / 状态 | `make up C=api,web,daemon` · `make down`（数据保留）· `make status` · `make destroy`（连数据删除） |
| 编译前端生产产物（秒开） | `pnpm --filter @multica/web build`（编译后 `make up` 自动走生产模式） |
| 强制以开发模式启动 | `MULTICA_WEB_DEV=1 make up C=api,web,daemon` |
| 看流水线进度 | 任务看板，或点开 issue 看实时运行输出 |
| 人工门拍板、改需求、追问 | Aris 聊天窗口 |
| 重跑某个阶段 | issue 页「重跑」，或 `make cli ARGS="issue rerun <编号>"` |
| 给角色加装 skill | 智能体 → 角色 → Skills（下次认领任务生效） |
| 手动放行评审门 | Aris 聊天里说，或 `make cli ARGS="pipeline advance --root <issueId> --stage <n>"` |
| 看智能体在不在干活 | 任务页右上角「N 个智能体工作中」；机器层 `tail -f ~/.multica/daemon.log` |

## 端到端验证

```bash
MULTICA_BASE_URL=... MULTICA_PAT=... MULTICA_WORKSPACE_ID=... \
E2E_ORCHESTRATOR_SESSION_ID=<bootstrap 打印的会话 id> \
  bash scripts/aris/e2e_vla_pipeline.sh
```

以 VLA（视觉-语言-动作）主题跑通 17 项检查：模板实例化、依赖门控、评审门 advance、
元数据绑定、聊天桥接。

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/aris-paper-pipeline.md](docs/aris-paper-pipeline.md) | 设计全案：架构、数据模型、API、时序、测试矩阵 |
| [scripts/aris/README.md](scripts/aris/README.md) | 花名册、skill 绑定与实验监控约定 |
| [README.zh.md](README.zh.md) · [SELF_HOSTING.md](SELF_HOSTING.md) | 上游 Multica 平台介绍与生产部署 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 上游开发流程与 worktree 操作 |

## License

跟随上游 Multica，见 [LICENSE](LICENSE)。
