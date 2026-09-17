#!/usr/bin/env bash
# cf-opt-adguard 多菜单安装管理脚本
# 功能：安装二进制 + 交互配置 + CFST 调速包装 + systemd/cron 定时；支持本地包或 GitHub 拉取。
# 样式参考：references/install_nodeget.sh
set -e

APP_NAME="cf-opt-adguard"
SERVICE_NAME="cf-opt-adguard"
DEFAULT_INSTALL_DIR="/opt/cf-opt-adguard"
# GitHub releases 拉取基址（构建时 build-release.sh 会重新注入；此处为兜底默认）。
DEFAULT_RELEASE_BASE_URL="https://github.com/jayvzh/cf-opt-adguard/releases/latest/download"

action=""
arch=""
install_dir=""
arg_url=""          # --url 覆盖拉取基址
skip_schedule=false

# 交互答案（parse_args 可预置，do_install 中逐项补默认）
agh_url=""; agh_user=""; agh_pass=""
window=""; min_hits=""
interval_days=""; interval_hours=""
cfst_dir=""; cfst_cmd=""; resolvers=""

_red()   { echo -e "\033[0;31m$1\033[0m"; }
_yellow(){ echo -e "\033[0;33m$1\033[0m"; }
_blue()  { echo -e "\033[0;36m$1\033[0m"; }
_green() { echo -e "\033[0;32m$1\033[0m"; }

########################################
# 环境检测
########################################
detect_env() {
    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) _red "不支持的架构: $(uname -m)（仅支持 amd64/arm64）"; exit 1 ;;
    esac
    SYSTEMD=0
    [ -d /run/systemd/system ] && SYSTEMD=1
}

is_root() { [ "$(id -u)" = "0" ]; }

release_base_url() {
    if [ -n "$arg_url" ]; then echo "$arg_url"; return; fi
    echo "$DEFAULT_RELEASE_BASE_URL"
}

########################################
# 获取二进制：本地二进制 > 本地 tar.gz > 远程拉取
########################################
fetch_binary() {
    local dest_dir="$1" src tarball
    local script_dir
    script_dir=$(cd "$(dirname "$0")" && pwd)

    if [ -x "$script_dir/cf-opt-adguard" ]; then
        src="$script_dir/cf-opt-adguard"
        _blue "使用同目录二进制: $src"
        cp "$src" "$dest_dir/cf-opt-adguard"
        return 0
    fi

    tarball=$(ls "$script_dir"/cf-opt-adguard-*-linux-*.tar.gz 2>/dev/null | head -n1 || true)
    if [ -n "$tarball" ]; then
        _blue "使用同目录发布包: $tarball"
        tar -xzf "$tarball" -C "$dest_dir" --strip-components=1 cf-opt-adguard*-linux-*/cf-opt-adguard
        return 0
    fi

    local base; base=$(release_base_url)
    if [ -z "$base" ]; then
        _red "未找到本地二进制/发布包，且未配置拉取地址。"
        _yellow "请把发布 tar.gz 或解压后的二进制与本脚本放同一目录，或用 --url 指定拉取基址。"
        exit 1
    fi
    local repo_root="${base%%/releases*}"
    local tag
    tag=$(curl -sI "$repo_root/releases/latest" | sed -n 's#.*tag/\(.*\)\r#\1#p')
    if [ -z "$tag" ]; then _red "获取最新版本号失败: $repo_root"; exit 1; fi
    tarball="cf-opt-adguard-${tag}-linux-${arch}.tar.gz"
    _blue "拉取发布包: $base/$tarball"
    curl -fL "$base/$tarball" -o "$dest_dir/$tarball"
    tar -xzf "$dest_dir/$tarball" -C "$dest_dir" --strip-components=1 \
        "cf-opt-adguard-${tag}-linux-${arch}/cf-opt-adguard"
    rm -f "$dest_dir/$tarball"
}

########################################
# 配置读写（settings.env 保存全部答案，供重配置/重装复用）
########################################
settings_file() { echo "$install_dir/settings.env"; }

load_settings() {
    local f; f=$(settings_file)
    [ -f "$f" ] && . "$f" || true
}

# 单引号包裹（shell 安全），YAML 用同函数 + yaml_quote
sh_quote() { local s=$1; s=${s//\'/\'\\\'\'}; printf "'%s'" "$s"; }
# YAML 单引号转义：' → ''
yaml_quote() { local s=$1; s=${s//\'/\'\'}; printf "'%s'" "$s"; }

ask_defaults() {
    # 已有 settings.env 的旧值做默认，回车沿用。
    agh_url=${agh_url:-${AGH_URL:-}}
    agh_user=${agh_user:-${AGH_USER:-admin}}
    agh_pass=${agh_pass:-${AGH_PASS:-}}
    window=${window:-${WINDOW:-7d}}
    min_hits=${min_hits:-${MIN_HITS:-20}}
    interval_days=${interval_days:-${INTERVAL_DAYS:-0}}
    interval_hours=${interval_hours:-${INTERVAL_HOURS:-12}}
    cfst_dir=${cfst_dir:-${CFST_DIR:-$install_dir/cfst}}
    cfst_cmd=${cfst_cmd:-${CFST_CMD:-./cfst -tl 200 -dn 20}}
    resolvers=${resolvers:-${RESOLVERS:-223.5.5.5,119.29.29.29}}
}

prompt_answers() {
    # 非交互模式（stdin 非 tty）且必填项齐全时跳过提问，直接校验。
    if [ ! -t 0 ] && [ -n "$agh_url" ] && [ -n "$agh_pass" ]; then
        _blue "==> 非交互模式，使用命令行参数与默认值"
        validate_answers
        return
    fi
    echo
    _blue "—— AdGuard Home 连接 ——"
    while [ -z "$agh_url" ]; do
        read -rp "AGH 地址（如 http://192.168.1.2:3000）: " agh_url || { _red "输入流已关闭且未提供 AGH 地址"; exit 1; }
    done
    read -rp "AGH 用户名（默认 $agh_user）: " _u; agh_user=${_u:-$agh_user}
    if [ -z "$agh_pass" ]; then
        printf "AGH 密码: "; read -rs agh_pass; echo
    fi
    read -rp "统计窗口 24h/7d/30d（默认 $window）: " _w; window=${_w:-$window}

    echo
    _blue "—— 聚合与调度 ——"
    read -rp "点击频次 min-hits（默认 $min_hits）: " _m; min_hits=${_m:-$min_hits}
    read -rp "运行间隔（天 小时，一行两个数，如 '0 6'=每6小时, '1 0'=每天，默认 $interval_days $interval_hours）: " _d _h
    interval_days=${_d:-$interval_days}; interval_hours=${_h:-$interval_hours}

    echo
    _blue "—— CloudflareSpeedTest ——"
    read -rp "cfst 目录（默认 $cfst_dir）: " _c; cfst_dir=${_c:-$cfst_dir}
    read -rp "cfst 命令（默认 $cfst_cmd）: " _cmd; cfst_cmd=${_cmd:-$cfst_cmd}
    read -rp "独立 resolver，逗号分隔（默认 $resolvers，勿指向本机 AGH）: " _r; resolvers=${_r:-$resolvers}

    validate_answers
}

validate_answers() {
    case "$window" in 24h|7d|30d|2w) ;; *) _red "统计窗口非法: $window（支持 24h/7d/30d/2w）"; exit 1 ;; esac
    case "$min_hits" in ''|*[!0-9]*) _red "点击频次必须为正整数: $min_hits"; exit 1 ;; esac
    [ "$min_hits" -ge 1 ] 2>/dev/null || { _red "点击频次必须 >= 1"; exit 1; }
    case "$interval_days$interval_hours" in ''|*[!0-9]*) _red "运行间隔非法（示例: 0 6）"; exit 1 ;; esac
    if [ $((interval_days * 24 + interval_hours)) -lt 1 ]; then
        _red "运行间隔至少 1 小时"; exit 1
    fi
    [ -x "$cfst_dir/cfst" ] || _yellow "警告: $cfst_dir/cfst 不存在或不可执行，请先下载 CloudflareSpeedTest 到该目录"
}

total_hours() { echo $((interval_days * 24 + interval_hours)); }

save_settings() {
    cat > "$(settings_file)" <<EOF
AGH_URL=$(sh_quote "$agh_url")
AGH_USER=$(sh_quote "$agh_user")
AGH_PASS=$(sh_quote "$agh_pass")
WINDOW=$(sh_quote "$window")
MIN_HITS=$(sh_quote "$min_hits")
INTERVAL_DAYS=$(sh_quote "$interval_days")
INTERVAL_HOURS=$(sh_quote "$interval_hours")
CFST_DIR=$(sh_quote "$cfst_dir")
CFST_CMD=$(sh_quote "$cfst_cmd")
RESOLVERS=$(sh_quote "$resolvers")
EOF
    chmod 600 "$(settings_file)"
}

########################################
# 生成 config.yaml / run-opt.sh
########################################
gen_config() {
    local resolvers_yaml="["
    local _sep="" r
    local IFS=','
    for r in $resolvers; do
        r=$(echo "$r" | xargs) # 去首尾空格
        resolvers_yaml="${resolvers_yaml}${_sep}\"$r\""
        _sep=", "
    done
    unset IFS
    resolvers_yaml="$resolvers_yaml]"

    cat > "$install_dir/config.yaml" <<EOF
# cf-opt-adguard 配置（由 install.sh 生成；密码为明文 600 权限，
# 可改为 password: \${AGH_PASSWORD} 形式并在运行环境注入变量）
adguard:
  url: $(yaml_quote "$agh_url")
  username: $(yaml_quote "$agh_user")
  password: $(yaml_quote "$agh_pass")
  timeout: 10s

querylog:
  window: $window
  page_size: 500
  max_pages: 200
  fetch_timeout: 5m

aggregate:
  mode: zone
  min_hits_24h: $min_hits
  min_hits_7d: $min_hits
  max_domains: 1000

detector:
  resolvers: $resolvers_yaml
  concurrency: 8
  timeout: 3s
  http_enabled: true
  score_threshold: 4

cfip:
  source: cfst:$cfst_dir/result.csv
  strategy: lowest_latency
  ip_versions: [4]

sync:
  mode: rewrite-api
  wildcard: true
  ttl: 720h
  rate_limit: 200ms
  retry: 3

runtime:
  db_path: $install_dir/data/state.db
  apply: true
  log_level: info
EOF
    chmod 600 "$install_dir/config.yaml"
}

gen_wrapper() {
    local th; th=$(total_hours)
    cat > "$install_dir/run-opt.sh" <<EOF
#!/usr/bin/env bash
# cf-opt-adguard 定时包装：先 CFST 测速 → 再流水线（--apply）。由 install.sh 生成。
set -euo pipefail
INSTALL_DIR=$(sh_quote "$install_dir")
CFST_DIR=$(sh_quote "$cfst_dir")
CFST_CMD=$(sh_quote "$cfst_cmd")
LOG_DIR="\$INSTALL_DIR/logs"
mkdir -p "\$LOG_DIR"
exec >> >(tee -a "\$LOG_DIR/run.log") 2>&1

echo "=== \$(date '+%F %T') CFST 测速开始（间隔 ${th}h）==="
cd "\$CFST_DIR"
eval "\$CFST_CMD"   # 测速失败即中止，不复用旧 result.csv
echo "=== \$(date '+%F %T') 流水线同步开始 ==="
cd "\$INSTALL_DIR"
./cf-opt-adguard run -c config.yaml --apply
echo "=== \$(date '+%F %T') 完成 ==="
EOF
    chmod 700 "$install_dir/run-opt.sh"
}

########################################
# 定时任务：systemd 优先，回退 cron
########################################
install_schedule() {
    [ "$skip_schedule" = true ] && { _yellow "已跳过定时任务安装（--skip-schedule）"; return 0; }
    if ! is_root; then
        _yellow "非 root，无法安装定时任务；可稍后手动执行: sudo bash $0 toggle"; return 0
    fi
    if [ "$SYSTEMD" = 1 ]; then install_schedule_systemd; else install_schedule_cron; fi
}

install_schedule_systemd() {
    local th; th=$(total_hours)
    cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=cf-opt-adguard: CFST 测速 + AdGuard Home rewrite 同步
[Service]
Type=oneshot
ExecStart=$install_dir/run-opt.sh
Nice=10
EOF
    cat > "/etc/systemd/system/${SERVICE_NAME}.timer" <<EOF
[Unit]
Description=cf-opt-adguard 定时触发（每 ${th} 小时）
[Timer]
OnBootSec=10min
OnUnitActiveSec=${th}h
Persistent=true
Unit=${SERVICE_NAME}.service
[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload
    systemctl enable --now "${SERVICE_NAME}.timer"
    _green "[ok] systemd timer 已启用（每 ${th} 小时）: systemctl list-timers ${SERVICE_NAME}"
}

# cron 仅支持整除 24 的小时步长与整天步长，其余值向上就近取整并提示。
interval_to_cron() {
    local h=$1 d up
    if [ "$h" -ge 13 ]; then
        d=$(( (h + 23) / 24 )) # 向上取整天
        [ $((d * 24)) -ne "$h" ] && _yellow "提示: cron 按天调度，${h}h 已取整为每 ${d} 天（4 点档）"
        echo "0 4 */$d * *"
        return
    fi
    if [ $((24 % h)) -eq 0 ]; then
        echo "0 */$h * * *"
        return
    fi
    up=12
    for d in 1 2 3 4 6 8 12; do
        if [ "$d" -ge "$h" ]; then up=$d; break; fi
    done
    _yellow "提示: cron 不支持每 ${h} 小时，已就近调整为每 ${up} 小时"
    echo "0 */$up * * *"
}

install_schedule_cron() {
    local spec entry cron_file="/etc/cron.d/${SERVICE_NAME}"
    spec=$(interval_to_cron "$(total_hours)")
    entry="$spec root $install_dir/run-opt.sh >> $install_dir/logs/cron.log 2>&1"
    {
        echo "SHELL=/bin/bash"
        echo "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
        echo "$entry"
    } > "$cron_file"
    chmod 644 "$cron_file"
    _green "[ok] cron 已写入 $cron_file（$spec）"
}

schedule_enabled() {
    if [ "$SYSTEMD" = 1 ]; then
        systemctl is-enabled "${SERVICE_NAME}.timer" >/dev/null 2>&1
    elif [ -f "/etc/cron.d/${SERVICE_NAME}" ]; then
        grep -qE '^[0-9*]' "/etc/cron.d/${SERVICE_NAME}"
    else
        return 1
    fi
}

remove_schedule() {
    if is_root; then
        if [ "$SYSTEMD" = 1 ]; then
            systemctl disable --now "${SERVICE_NAME}.timer" 2>/dev/null || true
            rm -f "/etc/systemd/system/${SERVICE_NAME}.timer" "/etc/systemd/system/${SERVICE_NAME}.service"
            systemctl daemon-reload
        fi
        rm -f "/etc/cron.d/${SERVICE_NAME}"
    fi
}

########################################
# 菜单动作
########################################
do_install() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if [ -d "$install_dir" ] && [ -f "$install_dir/config.yaml" ]; then
        _yellow "检测到已有安装: $install_dir（将保留 settings.env 并重装）"
        load_settings
    fi
    ask_defaults
    prompt_answers

    echo
    _blue "==> 安装目录: $install_dir"
    case "$install_dir" in /opt/*|/etc/*|/usr/*|/var/*)
        is_root || { _red "写入 $install_dir 需要 root，请用 sudo 运行，或 --install-dir 指定用户目录。"; exit 1; } ;;
    esac
    mkdir -p "$install_dir/logs" "$install_dir/data"

    fetch_binary "$install_dir"
    chmod +x "$install_dir/cf-opt-adguard"

    save_settings
    gen_config
    gen_wrapper
    install_schedule

    echo
    _green "✅ 安装完成"
    _green "  目录: $install_dir"
    _green "  配置: $install_dir/config.yaml"
    _green "  包装: $install_dir/run-opt.sh（cfst 测速 → run --apply）"
    _green "  间隔: 每 $(total_hours) 小时（${interval_days}d ${interval_hours}h）"
    _green "  日志: $install_dir/logs/run.log"
    echo
    _y="n"
    if [ -t 0 ]; then read -rp "是否立即运行一次验证？(y/N): " _y || _y="n"; fi
    if [ "$_y" = "y" ]; then do_run_once; fi
}

do_run_once() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if [ ! -x "$install_dir/run-opt.sh" ]; then
        _red "未找到 $install_dir/run-opt.sh，请先执行安装（菜单 1）。"; return 1
    fi
    bash "$install_dir/run-opt.sh"
}

do_reconfig() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    [ -d "$install_dir" ] || { _red "未安装: $install_dir"; return 1; }
    [ -f "$(settings_file)" ] && load_settings
    ask_defaults
    prompt_answers
    save_settings
    gen_config
    gen_wrapper
    if [ "$skip_schedule" != true ]; then remove_schedule; install_schedule; fi
    _green "✅ 配置已更新并重载定时任务"
}

do_toggle() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    if ! is_root; then _red "启停定时任务需要 root，请用 sudo 运行。"; return 1; fi
    if schedule_enabled; then
        if [ "$SYSTEMD" = 1 ]; then
            systemctl disable --now "${SERVICE_NAME}.timer"
        else
            sed -i 's/^\([0-9*]\)/# \1/' "/etc/cron.d/${SERVICE_NAME}"
        fi
        _yellow "⏸ 定时任务已暂停"
    else
        if [ "$SYSTEMD" = 1 ]; then
            systemctl enable --now "${SERVICE_NAME}.timer"
        else
            sed -i 's/^# \([0-9*]\)/\1/' "/etc/cron.d/${SERVICE_NAME}"
        fi
        _green "▶ 定时任务已启用"
    fi
}

do_status() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    echo
    if [ -x "$install_dir/cf-opt-adguard" ]; then
        "$install_dir/cf-opt-adguard" version
    else
        _red "未安装或二进制缺失: $install_dir/cf-opt-adguard"
    fi
    echo
    if schedule_enabled; then
        _green "定时任务: 启用"
    else
        _yellow "定时任务: 停用/未安装"
    fi
    if [ "$SYSTEMD" = 1 ] && is_root && systemctl list-unit-files "${SERVICE_NAME}.timer" >/dev/null 2>&1; then
        systemctl list-timers "${SERVICE_NAME}.timer" --no-pager 2>/dev/null || true
    fi
    echo
    local log="$install_dir/logs/run.log"
    if [ -f "$log" ]; then
        _blue "—— 最近日志（tail -n 40 $log）——"
        tail -n 40 "$log"
    else
        _yellow "尚无运行日志: $log"
    fi
}

do_update() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    [ -d "$install_dir" ] || { _red "未安装: $install_dir"; return 1; }
    load_settings
    _blue "==> 更新二进制（保留配置与日志）"
    fetch_binary "$install_dir"
    chmod +x "$install_dir/cf-opt-adguard"
    if is_root && [ "$SYSTEMD" = 1 ] && schedule_enabled; then
        systemctl start "${SERVICE_NAME}.service" || true
    fi
    _green "✅ 更新完成: $("$install_dir/cf-opt-adguard" version)"
}

do_uninstall() {
    install_dir=${install_dir:-$DEFAULT_INSTALL_DIR}
    remove_schedule
    if [ -d "$install_dir" ]; then
        rm -rf "$install_dir"
        _green "✅ 已卸载：定时任务已移除，$install_dir 已删除"
    else
        _yellow "未发现安装目录 $install_dir，仅清理定时任务。"
    fi
}

########################################
# 菜单 / 参数 / 主循环
########################################
menu() {
    echo
    echo "================================"
    echo "     cf-opt-adguard 管理脚本"
    echo "================================"
    echo
    echo "1. 安装 / 重新安装（含配置与定时任务）"
    echo "2. 立即运行一次（cfst 测速 → 同步）"
    echo "3. 修改配置（AGH/窗口/频次/间隔等）"
    echo "4. 启用 / 暂停 定时任务"
    echo "5. 查看状态与最近日志"
    echo "6. 更新二进制"
    echo "7. 卸载"
    echo
    echo "0. 退出"
    echo
    read -rp "请输入选项: " choice
    case "$choice" in
        1) action="install" ;;
        2) action="run-once" ;;
        3) action="reconfig" ;;
        4) action="toggle" ;;
        5) action="status" ;;
        6) action="update" ;;
        7) action="uninstall" ;;
        0) exit 0 ;;
        *) return ;;
    esac
    dispatch "$action"
}

parse_args() {
    action="$1"; shift || true
    while [ $# -gt 0 ]; do
        case "$1" in
            --agh-url)     agh_url="$2"; shift 2 ;;
            --agh-user)    agh_user="$2"; shift 2 ;;
            --agh-pass)    agh_pass="$2"; shift 2 ;;
            --window)      window="$2"; shift 2 ;;
            --min-hits)    min_hits="$2"; shift 2 ;;
            --interval)
                if [[ "$2" == *" "* ]]; then
                    interval_days="${2%% *}"; interval_hours="${2#* }"
                else
                    interval_days=0; interval_hours="$2"
                fi
                shift 2 ;;
            --cfst-dir)    cfst_dir="$2"; shift 2 ;;
            --cfst-cmd)    cfst_cmd="$2"; shift 2 ;;
            --resolvers)   resolvers="$2"; shift 2 ;;
            --install-dir) install_dir="$2"; shift 2 ;;
            --url)         arg_url="$2"; shift 2 ;;
            --skip-schedule) skip_schedule=true; shift ;;
            *) _red "未知参数: $1"; exit 1 ;;
        esac
    done
}

dispatch() {
    case "$1" in
        install)   do_install ;;
        run-once)  do_run_once ;;
        reconfig)  do_reconfig ;;
        toggle)    do_toggle ;;
        status)    do_status ;;
        update)    do_update ;;
        uninstall) do_uninstall ;;
        *) usage ;;
    esac
}

usage() {
cat <<EOF
Usage:
  install.sh [command] [options]

Commands:
  install     安装（二进制 + 配置 + 定时任务，缺省进入交互）
  run-once    立即运行一次（cfst 测速 → 同步）
  reconfig    修改配置并重载定时任务
  toggle      启用 / 暂停 定时任务
  status      查看版本 / 定时状态 / 最近日志
  update      更新二进制（保留配置与日志）
  uninstall   卸载（移除定时任务并删除安装目录）
  help        本帮助

Options:
  --agh-url <url>        AdGuard Home 地址（如 http://192.168.1.2:3000）
  --agh-user <user>      AGH 用户名（默认 admin）
  --agh-pass <pass>      AGH 密码
  --window <w>           统计窗口 24h/7d/30d/2w（默认 7d）
  --min-hits <n>         点击频次阈值（默认 20）
  --interval "<d h>"     运行间隔，天数 小时（如 "0 6"=每6小时, "1 0"=每天；默认 "0 12"）
  --cfst-dir <dir>       CFST 目录（默认 <安装目录>/cfst）
  --cfst-cmd <cmd>       CFST 命令（默认 "./cfst -tl 200 -dn 20"）
  --resolvers <a,b>      独立 resolver（默认 223.5.5.5,119.29.29.29）
  --install-dir <dir>    安装目录（默认 /opt/cf-opt-adguard）
  --url <base>           发布包拉取基址（.../releases/latest/download）
  --skip-schedule        只落文件不装定时任务（测试用）

示例:
  sudo bash install.sh                        # 交互菜单
  sudo bash install.sh install --agh-url http://192.168.1.2:3000 \\
      --agh-pass 'xxx' --interval "1 0"       # 非交互安装
EOF
}

detect_env
if [ $# -gt 0 ]; then
    parse_args "$@"
    dispatch "$action"
else
    while true; do
        menu
    done
fi
