# Release v0.1.0 — 首个公开发布

> AdGuard Home × Cloudflare 优选 IP 自动编排器：从 AGH 查询日志挖掘高频域名，独立探测确认 Cloudflare CDN，自动维护 DNS 重写规则。

## 功能亮点

- **全链路自动编排**：AGH querylog 只读采集 → 频次聚合 → 一级域归并（zone 内证据充分才下发通配，混入不确定域名自动回退逐条精确，防误伤）→ CF 探测确认 → 优选 IP → 增量计划 → AGH Rewrite 写入 → DNS 回查验证。
- **独立 CF 探测**：DNS CNAME 指向 + Cloudflare IP 段归属 + HTTP 响应头三信号打分，不轻信单一特征。
- **优选 IP 接入 CloudflareSpeedTest**：直接读取 CFST 的 `result.csv`，v4/v6 区分，多 IP 择优。
- **安全默认**：`run` 默认 dry-run 只打印计划，`--apply` 才写入；测速失败自动中止本轮，不复用旧结果。
- **一键安装管理脚本**（install.sh）：
  - 交互菜单：安装 / 立即运行 / 重配置 / 定时启停 / 状态日志 / 更新 / 卸载
  - 支持 GitHub Release 拉取或本地包安装（amd64 / arm64）
  - systemd timer 优先（任意小时数精确调度），无 systemd 自动回退 cron
  - 定时编排：先 CFST 测速生成 `result.csv`，再执行同步流水线

## 下载

| 文件 | 平台 |
| --- | --- |
| `cf-opt-adguard-v0.1.0-linux-amd64.tar.gz` | Linux x86_64 |
| `cf-opt-adguard-v0.1.0-linux-arm64.tar.gz` | Linux ARM64 |
| `checksums.txt` | SHA256 校验和 |

包内含：`cf-opt-adguard` 主二进制、`install.sh` 安装管理脚本、`config.example.yaml` 配置样例。

## 快速开始

```bash
# 1. 下载并解压（按架构选择）
wget https://github.com/<OWNER>/<REPO>/releases/download/v0.1.0/cf-opt-adguard-v0.1.0-linux-amd64.tar.gz
tar -xzf cf-opt-adguard-v0.1.0-linux-amd64.tar.gz && cd cf-opt-adguard-v0.1.0-linux-amd64

# 2. 交互式安装（含配置向导与定时任务）
sudo bash install.sh
```

安装向导会依次询问：AGH 地址与凭据、统计窗口（24h/7d/30d/2w）、点击频次阈值、运行间隔（一行输入"天 小时"，如 `0 6` = 每 6 小时、`1 0` = 每天）、CFST 目录与测速命令（默认 `./cfst -tl 200 -dn 20`）。

非交互安装示例：

```bash
sudo bash install.sh install \
    --agh-url http://192.168.1.2:3000 --agh-user admin --agh-pass 'xxx' \
    --window 24h --min-hits 20 --interval "0 6" \
    --cfst-dir /opt/cfst --cfst-cmd "./cfst -tl 200 -dn 20"
```

## 前置要求

- 已运行 AdGuard Home（需管理员账号，REST API 可达）
- 已下载 [CloudflareSpeedTest](https://github.com/XIU2/CloudflareSpeedTest) 并放到 cfst 目录（与 `cfst` 二进制同级）
- Linux amd64 / arm64，systemd（可选，无则回退 cron）
- 独立 DNS resolver（默认 223.5.5.5 / 119.29.29.29，勿指向本机 AGH，避免探测回路）

## 注意事项

- 首次使用建议先在测试实例验证：手动跑一次 dry-run 核对计划无误后再启用 `--apply` 定时同步。
- 安装目录默认 `/opt/cf-opt-adguard`；`config.yaml` 含明文密码（600 权限），可改用 `${AGH_PASSWORD}` 环境变量形态。
- 卸载默认备份式删除（`<安装目录>.bak.时间戳`），加 `--purge` 彻底删除。

## 免责声明

本工具修改家庭 / 内网 DNS 解析行为，误判可能影响个别站点访问，请在理解工作原理后使用，作者不承担因使用本工具产生的任何后果。
