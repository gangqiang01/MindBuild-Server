#!/bin/bash
export WEKNORA_SANDBOX_DOCKER_ENABLED=true

# 可选：自定义沙箱镜像（默认 wechatopenai/weknora-sandbox:latest）
export WEKNORA_SANDBOX_DOCKER_IMAGE=wechatopenai/weknora-sandbox:v0.9.0

# 单脚本超时（秒）
export WEKNORA_SANDBOX_TIMEOUT=60
