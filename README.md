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

### 0. 前置要求

Go ≥ 1.26（首次构建自动拉取 toolchain）、Node 20+ 与 pnpm、Docker、Python 3。

### 1. 启动平台

```bash
cp .env.example .env          # 按需修改
make up C=api,web,daemon      # 起 API、前端和本地 daemon
make status                   # 查看本 checkout 分到的地址（形如 web :13436 / api :18516）
```

浏览器打开 `make status` 显示的 web 地址，注册账号（dev 模式的邮箱验证码打印在
`~/.multica/dev/envs/<env>/logs/api.log`）。

### 2. 接入智能体运行时

在本机安装至少一个受支持的 agent CLI（Claude Code、Codex、Antigravity 等）。
daemon 由 `make up` 拉起，会自动探测本机 CLI；到网页 **运行时** 页确认在线。
日志在 `~/.multica/daemon.log`。

### 3. 一键配置 ARIS 花名册

```bash
# 网页「设置 → API Tokens」创建个人访问令牌（mul_ 开头），只放环境变量，不要写进文件
export MULTICA_BASE_URL=http://localhost:<api端口>
export MULTICA_PAT=mul_xxx
export MULTICA_WORKSPACE_ID=<工作区 uuid>
python3 scripts/aris/bootstrap.py
```

脚本会（幂等，可重复执行）：上架并绑定 86 个 ARIS skills、创建 8 个角色智能体
（Aris/Scout/Researcher/Planner/Experimenter/Analyst/Writer/Reviewer）、
创建 `paper-default` 流水线模板、打开主理人聊天会话并打印 `orchestrator_session_id`。

### 4. 开始写论文

网页 → **聊天** → **Aris**，一句话描述研究目标。建议带上：
已有代码/数据路径、目标会议或期刊、算力约束、是否有真机。

Aris 会先和你确认约束，然后实例化流水线、Stage 1 自动开跑。之后你只需要：

- 在聊天里收进展汇报；
- 在人工门拍板（它会给出选项）；
- 随时插话改需求、追问细节。

## 日常操作

| 想做什么 | 去哪里 |
| --- | --- |
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
