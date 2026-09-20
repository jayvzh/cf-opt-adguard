# cf-opt-adguard

一个本地运行的 **AdGuard Home × Cloudflare 优选 IP 自动编排器**：从 AGH 查询日志挖掘高频域名，独立探测确认 Cloudflare CDN，把优选 IP 增量写为 AGH 的 DNS 重写规则，并持续维护（新站点自动加入、失效站点自动淘汰）。

## 功能亮点

- **全链路自动编排**：AGH querylog 只读采集 → 频次聚合 → 一级域归并（zone 内证据充分才下发通配，混入不确定域名自动回退逐条精确，防误伤）→ CF 探测确认 → 优选 IP → 增量计划 → AGH Rewrite 写入 → DNS 回查验证。
- **独立 CF 探测**：DNS CNAME 指向 + Cloudflare IP 段归属 + HTTP 响应头三信号打分，不轻信单一特征。
- **优选 IP 接入 CloudflareSpeedTest**：直接读取 CFST 的 `result.csv`，v4/v6 区分，多 IP 择优。
- **安全默认**：`run` 默认 dry-run 只打印计划，`--apply` 才写入；测速失败自动中止本轮，不复用旧结果。

## 它如何工作

```text
AGH 查询日志 → 采集 → 频次聚合 → CF 探测（独立 DNS + IP 段 + HTTP 头打分）
           → 优选 IP（CloudflareSpeedTest 结果文件（默认）/ 优选域名 / 静态列表）
           → 增量计划（add/update/remove）→ AGH Rewrite API → DNS 回查验证
```

- 外部独立 CLI，不改 AGH 核心、不做 AGH 插件；
- 默认按可注册域（zone）混合归并：zone 内证据充分才下发 `*.zone` 通配，混入不确定域名自动回退逐条精确，防误伤（D14）；默认 dry-run 预览，全程本地运行、不上传查询日志；

## 部署与使用


### 方式一：手动运行

默认优选 IP 来源为 CFST 的 `result.csv`，把主二进制放进 CFST 目录（与 `cfst` 同级），复制 `config.example.yaml` 为 `config.yaml` 并按注释修改：

```bash
cd /opt/cfst                                # cfst 目录：cfst、cf-opt-adguard、config.yaml 同级

./cfst -tl 200 -dn 20                       # 1. 测速生成 result.csv（想更新优选 IP 时重跑）

./cf-opt-adguard run -c config.yaml         # 2. dry-run 核对计划（默认不写入 AGH）

./cf-opt-adguard run -c config.yaml --apply # 3. 确认无误后写入
                                            #    终端运行会先列出 CF 域名清单，输入 y 确认写入
                                            #    （其他输入取消，退出码 5；--yes 可跳过确认）
```


### 方式二：安装脚本（可安装定时任务）

一条命令拉取脚本并进入交互菜单（会自动下载最新发布包并安装，需联网）：

```bash
bash <(curl -sL https://github.com/jayvzh/cf-opt-adguard/raw/refs/heads/main/scripts/install.sh)
```

> 注意必须是进程替换 `<(...)`，不能写成 `curl ... | bash`——直管道会占住标准输入导致向导无法回答。脚本检测到这种误用时会报错并给出正确命令。

安装向导第 0 步先确认**安装目录**（直接回车使用默认 `/opt/cf-opt-adguard`，已安装过则默认沿用旧路径），随后依次询问：AGH 地址与凭据（输完立即连接预检，失败原地重输）、统计窗口（24h/7d/30d/2w）、点击频次阈值、运行间隔（一行输入"天 小时"，如 `0 6` = 每 6 小时、`1 0` = 每天）、CFST 目录与测速命令（默认 `./cfst -tl 200 -dn 20`）、独立 resolver。所有带默认值的项**直接回车即采用默认值**（提示中以 `[当前默认]` 标注），非法输入会报错并要求重新输入；若目录中没有 cfst，会询问是否自动从 [CloudflareSpeedTest 官方 Release](https://github.com/XIU2/CloudflareSpeedTest/releases) 下载最新版（amd64/arm64 自动匹配）。

安装完成后会额外创建**快捷命令** `cf-opt-adguard`（软链到安装目录内的管理脚本副本，非 root 安装则跳过并提示手动方式），任意目录直接运行即可打开管理菜单：

```bash
cf-opt-adguard               # 打开管理菜单（首行一行状态：版本 ｜ 同步间隔 ｜ 定时状态 ｜ 安装路径）
cf-opt-adguard run-sync      # 仅运行同步（使用现有测速结果，跳过 CFST 测速）
cf-opt-adguard run-once      # 立即跑一轮（测速命令确认 → 配置确认 → 域名清单确认 → 同步）
cf-opt-adguard logs          # 查看运行日志（run.log 尾部）
cf-opt-adguard cron          # 定时任务管理（启用状态/上次运行/暂停/重配间隔/卸载定时）
cf-opt-adguard reconfig      # 修改配置并重载定时任务
cf-opt-adguard help          # 全部子命令
```

也可以再次运行安装命令进入同一管理菜单。未安装时菜单只有「安装 / 帮助」两项；已安装后菜单分组如下（首行直接显示一行简单状态信息）：

| 菜单 | 子命令 | 说明 |
| --- | --- | --- |
| 1 | `run-sync` | 仅运行同步：使用现有 `result.csv`，跳过 CFST 测速 |
| 2 | `run-once` | 立即运行一次：测速命令确认（可修改参数）→ 配置确认 → 域名清单确认 → 同步 |
| 3 | `logs` | 查看运行日志（run.log 尾部，交互可指定行数） |
| 4 | `cron` | 定时任务管理：显示启用状态与上次运行记录，可暂停/启用、重新配置间隔、卸载定时任务 |
| 5 | `update` | 更新主程序二进制 |
| 6 | `install-cfst` | 安装 / 更新 CloudflareSpeedTest 依赖 |
| 7 | `reconfig` | 重新配置（AGH 地址/凭据、窗口、频次阈值、间隔等）并重载定时任务 |
| 8 | `uninstall` | 卸载（移除定时任务、快捷命令并删除安装目录） |
| 9 | `help` | 全部子命令 |

定时任务：systemd 可用时注册 `cf-opt-adguard.service + .timer`（任意小时数精确）；否则回退 `/etc/cron.d/`（仅支持整点整除或整天步长，其他值就近取整并提示）。

非交互安装（全部参数见 `bash install.sh help`）：

```bash
sudo bash <(curl -sL https://github.com/jayvzh/cf-opt-adguard/raw/refs/heads/main/scripts/install.sh) \
    install --agh-url http://192.168.1.2:3000 --agh-user admin --agh-pass 'xxx' \
    --window 24h --min-hits 10 --interval "0 6" \
    --cfst-dir /opt/cfst --with-cfst       # --with-cfst 顺带自动下载 cfst；--skip-schedule 可先不装定时器
```

## 免责声明

本工具修改家庭 / 内网 DNS 解析行为，误判可能影响个别站点访问；`run` 默认即为 dry-run（只打印计划不写入），请核对计划无误后再加 `--apply` 执行，建议先在测试实例验证。
