# kaze-sync

自托管文件同步服务: OpenWrt 或普通 Linux (systemd, OpenRC) 上的服务端 `kaze-syncd`, Windows 与 Linux 客户端 `kaze-syncc`. 设计见 `docs/design.md`, 实施计划与验证方式见 `docs/plan.md`; 开工前先读这两份, 再以实际代码为准.

- **首要目标: 低 CPU, 低内存, 低流量** (用户 2026-11-02): 这是本项目最大的特色, 设计与实现都先按它取舍, 服务端以 512 MB 内存的机器为目标; 细则见 `docs/design.md` 开头 "首要目标" 一节.
- **轻量原则**: 每个功能, 页面, 命令, 配置项和表都必须由 `docs/design.md` "轻量原则" 列出的某项用户需求直接要求, 否则不做. 能用一种机制解决的不用两种.
- **设计先定后做**: 改变设计 (协议, 表结构, 冲突与版本语义, 安全) 前先与用户讨论并更新 `docs/design.md`; 里程碑内容按 `docs/plan.md`.
- **验证**: 按 `docs/plan.md` "开发与验证方式": `go test ./...`, NTFS 相关测试按其中的 Windows 单元测试方式运行; 每个里程碑结束时单元, 模拟和端到端测试全部通过后提交一次.
- **临时文件**: 放在仓库内的 `.tmp/` (已被 Git 忽略), 确认不再需要后删除.
- **文档语言**: 简体中文, 使用 ASCII 标点, 与现有文档一致 (用户文档除外, 见下一条); 代码注释沿用英文.
- **用户文档**: 根 `README.md`, `README.zh-CN.md` 和 `docs/manuals/` 下的文档是给使用者看的, 中英文各一份, 默认英文 (用户 2026-11-04): `X.md` 为英文, `X.zh-CN.md` 为简体中文, 主标题后各有到另一语言的切换链接, 内容保持同步, 改一份时同一次提交改另一份; 中文版不混入英文句子 (命令, 路径, 标识符和 Web UI 上的原文除外). 其他文档 (`docs/design.md`, `docs/plan.md`, `docs/operations.md` 等) 不变, 仍只有简体中文. 参照 ARW 的做法: `~/projects/ARW/docs/agents/writing.md` 的读者文档一节.
