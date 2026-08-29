#!/bin/sh
# AetherLink 容器入口：先以 root 修正 /config 的属主，再降权运行主进程。
#
# 绑定挂载（./config:/config）时，宿主目录的属主会覆盖镜像里的设置，
# 通常是 root:root。容器内的非 root 进程无法在其中创建配置文件，
# 主进程会启动失败退出，配合 restart: always 就表现为容器反复重启。
set -e

config_path="${AETHERLINK_CONFIG:-/config/config.yaml}"
config_dir=$(dirname "$config_path")
puid="${PUID:-10001}"
pgid="${PGID:-10001}"

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$config_dir"
    if [ "$(stat -c %u "$config_dir")" != "$puid" ] || [ "$(stat -c %g "$config_dir")" != "$pgid" ]; then
        echo "[entrypoint] 将 $config_dir 的属主调整为 $puid:$pgid"
        chown -R "$puid:$pgid" "$config_dir" ||
            echo "[entrypoint] 警告：chown 失败（只读挂载或网络文件系统？），继续尝试启动"
    fi
    exec su-exec "$puid:$pgid" /aetherlink "$@"
fi

# 已经用 compose 的 user: 指定了非 root 身份，改不了属主，只能提示。
if [ ! -w "$config_dir" ]; then
    echo "[entrypoint] 警告：$config_dir 对当前用户 $(id -u):$(id -g) 不可写"
    echo "[entrypoint] 请在宿主上执行：chown -R $(id -u):$(id -g) ./config"
fi
exec /aetherlink "$@"