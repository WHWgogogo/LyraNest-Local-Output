# LyraNest Local Output（本机音频直出服务）

<p align="center">
  <img src="https://raw.githubusercontent.com/WHWgogogo/LyraNest/main/docs/images/lyranest-logo.png" alt="LyraNest Local Output" width="150" />
</p>

<p align="center">为 LyraNest 音乐服务提供 NAS 本机 3.5mm 耳机孔、主板音频口与 USB DAC 外置声卡母带级直出的官方插件。</p>

<p align="center">
  <a href="https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest"><img src="https://img.shields.io/github/v/release/WHWgogogo/LyraNest-Local-Output?display_name=tag&label=Release" alt="Latest Release" /></a>
  <a href="https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest"><img src="https://img.shields.io/badge/Platform-fnOS%20%7C%20Synology%20%7C%20QNAP%20%7C%20TerraMaster%20%7C%20UGNAS%20%7C%20CWNAS%20%7C%20Docker-4f46e5" alt="Platforms" /></a>
  <a href="https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest/download/docker-compose.yml"><img src="https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker&logoColor=white" alt="Docker Compose" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-orange.svg" alt="License: CC BY-NC-SA 4.0" /></a>
</p>

<p align="center">
  <a href="https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest">下载最新版</a> ·
  <a href="https://lyranest.cc.cd/">官网</a> ·
  <a href="#docker-compose-部署">Docker 部署</a> ·
  <a href="https://github.com/WHWgogogo/LyraNest">LyraNest 主项目</a>
</p>

> **插件说明**：本插件为 LyraNest 官方音频硬件扩展组件，专为拥有物理音频输出口（3.5mm 音频口、光纤 S/PDIF、USB 声卡或解码耳放一体机）的 NAS 或小主机设计。通过 Linux 底层 ALSA / MPD 驱动硬件发声。使用前请先部署 [LyraNest 主服务](https://github.com/WHWgogogo/LyraNest)。

当前稳定版本：`0.1.0`

交流 QQ 群：`700454910`

---

## 0.1.0 首发特性

- **母带级物理直通**：直接驱动主板集成声卡或外接 USB DAC 解码器，绕过任何网络二次压缩，提供零衰减的无损原音输出。
- **极简资源占用**：底层基于轻量 Go 服务与精简 MPD 架构，运行时常驻内存仅约 **13MB**，CPU 占用接近于 0。
- **智能硬件检测**：自动枚举系统底层声卡列表与混音控制通道（Mixer），支持一键播放音频测试音。
- **全端投送中心无缝聚合**：与 LyraNest 网页端、手机端 App 及 TV 端联动，直接作为「本机声卡」通道显示并支持全端音量调节。
- **全平台 NAS 原生安装支持**：全面覆盖飞牛 fnOS、群晖 DSM、铁威马 TOS 7、威联通 QNAP、绿联 UGnas 及畅网 CWNAS。

---

## 功能简介

- **NAS 变身 HiFi 播放机**：将闲置 NAS、HTPC 或迷你主机插上音箱或耳机，无需额外购买数播硬件。
- **USB DAC 免驱兼容**：即插即用主流 USB 外置声卡、小尾巴、解码器与耳放。
- **断点记忆与无缝接力**：在手机上听歌回到家，一键投送至 NAS 物理音箱继续播放，进度丝毫不差。
- **RESTful 控制与健康探测**：提供开箱即用的 HTTP API、测试播放路由（`/api/test-play`）与健康检查端点（`/healthz`）。

---

## 获取安装包与部署文件

请前往 [GitHub 最新发行版](https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest) 下载对应平台的文件：

| 文件 | 适用平台 / 架构 | 说明 |
| :--- | :--- | :--- |
| `LyraNest-Local-Output-0.1.0-fnos-x86.fpk` | 飞牛 fnOS (x86_64) | 飞牛 NAS x86 原生安装包（推荐） |
| `LyraNest-Local-Output-0.1.0-fnos-arm.fpk` | 飞牛 fnOS (ARM64) | 飞牛 NAS ARM 原生安装包 |
| `LyraNest-Local-Output-0.1.0-cwnas.cpk` | 畅网 NAS (CWNAS / AINAS) | 畅网私有云原生应用安装包 |
| `LyraNest-Local-Output-0.1.0-synology-x86_64.spk` | 群晖 DSM (x86_64) | 群晖 DSM 7.x 原生套件 |
| `LyraNest-Local-Output-0.1.0-synology-armv8.spk` | 群晖 DSM (ARM64) | 群晖 DSM 7.x 原生套件 |
| `LyraNest-Local-Output-0.1.0-terramaster-x86_64.deb` | 铁威马 TOS 7 (x86_64) | 铁威马应用中心原生安装包 |
| `LyraNest-Local-Output-0.1.0-terramaster-aarch64.deb`| 铁威马 TOS 7 (ARM64) | 铁威马应用中心原生安装包 |
| `LyraNest-Local-Output-0.1.0-qnap-x86_64.qpkg` | 威联通 QNAP (x86_64) | 威联通 App Center 原生套件 |
| `LyraNest-Local-Output-0.1.0-qnap-arm_64.qpkg` | 威联通 QNAP (ARM64) | 威联通 App Center 原生套件 |
| `LyraNest-Local-Output-0.1.0-ugnas-amd64.upk` | 绿联 NAS (AMD64) | 绿联私有云原生应用包 |
| `LyraNest-Local-Output-0.1.0-ugnas-arm64.upk` | 绿联 NAS (ARM64) | 绿联私有云原生应用包 |
| `docker-compose.yml` | 通用 Docker 环境 | Docker Compose 一键部署配置 |

---

## 飞牛 fnOS 原生 FPK 安装（推荐）

1. 从 [GitHub 最新发行版](https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest) 下载对应架构的 FPK：x86 设备使用 `fnos-x86.fpk`，ARM 设备使用 `fnos-arm.fpk`。
2. 在飞牛应用中心选择“手动安装 / 上传应用”，上传 FPK 完成安装。
3. 安装完成后后台服务自动常驻，监听 `8091` 端口；在 LyraNest 主程序「投送中心」即可直接看到「本机声卡」通道。

---

## 畅网 NAS (CWNAS / AINAS) 原生 CPK 安装

畅网 NAS 用户可下载 `LyraNest-Local-Output-0.1.0-cwnas.cpk`。在畅网 NAS 系统应用管理器中点击“手动安装 / 本地安装”，选择下载的 `.cpk` 文件即可一键部署并自动注册后台服务。

---

## 其他 NAS 原生安装包

QNAP、Synology DSM、绿联 NAS 与铁威马 TOS 7 用户可从 [GitHub 最新发行版](https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest) 下载对应架构的原生包，在各自系统的应用中心或套件中心选择手动安装：

- **威联通 QNAP**：启用“允许安装非 QNAP 签名的应用程序”，手动上传 `.qpkg`。
- **群晖 DSM**：打开套件中心点击“手动安装”，上传 `.spk` 安装。
- **绿联 NAS**：在应用中心选择“本地安装”，上传对应架构的 `.upk`。
- **铁威马 TOS 7**：在应用中心选择“手动安装”，上传对应架构的 `.deb`。

---

## Docker 镜像

服务端镜像统一发布至 GitHub Container Registry：

```text
ghcr.io/whwgogogo/lyranest-local-output:0.1.0
```

支持自动多架构自适应（`linux/amd64` 与 `linux/arm64`），默认提供 `0.1.0` 与 `latest` 标签。

---

## Docker Compose 部署

根目录提供了标准的 [`docker-compose.yml`](docker-compose.yml)：

> [!IMPORTANT]
> **声卡设备挂载说明**：
> Docker 部署必须将主机的物理音频设备 `/dev/snd` 映射至容器中（即 `devices: - "/dev/snd:/dev/snd"`）。若宿主机系统未检测到声卡硬件或驱动，容器启动时会按预期报错退出。

```yaml
services:
  local-output:
    image: ghcr.io/whwgogogo/lyranest-local-output:0.1.0
    container_name: lyranest-local-output
    restart: unless-stopped
    devices:
      - "/dev/snd:/dev/snd"
    ports:
      - "${LOCAL_OUTPUT_HOST_PORT:-8091}:8091"
    volumes:
      - "${LOCAL_OUTPUT_MUSIC_DIR:-./music}:/music:ro"
      - "${LOCAL_OUTPUT_DATA_DIR:-./data}:/data:rw"
    environment:
      LOCAL_OUTPUT_PORT: "8091"
      ALSA_CARD: "${ALSA_CARD:-0}"
      ALSA_PCM_DEVICE: "${ALSA_PCM_DEVICE:-0}"
      MPD_MIXER_CONTROL: "${MPD_MIXER_CONTROL:-Headphone}"
      LOCAL_OUTPUT_LOG_LEVEL: "info"
      TZ: "Asia/Shanghai"
    mem_limit: 64m
    healthcheck:
      test: ["CMD", "/usr/local/bin/local-output", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
```

### 部署与启动命令

```bash
mkdir -p lyranest-local-output && cd lyranest-local-output
curl -fLO https://github.com/WHWgogogo/LyraNest-Local-Output/releases/latest/download/docker-compose.yml
docker compose pull
docker compose up -d
curl http://127.0.0.1:8091/healthz
```

---

## 声卡探测与配置说明

如有多块声卡或外接 USB DAC，可在 NAS 终端查看声卡编号：

```bash
cat /proc/asound/cards
```

例如输出：
```text
 0 [PCH            ]: HDA-Intel - HDA Intel PCH
 1 [DAC            ]: USB-Audio - USB Audio DAC
```

若需使用声卡 1（USB DAC），只需在环境变量中设置 `ALSA_CARD=1` 并重启服务即可。

---

## 官方生态与项目友链

- [LyraNest 主项目](https://github.com/WHWgogogo/LyraNest)：LyraNest 官方全平台自托管音乐服务。
- [LyraNest AirPlay Bridge](https://github.com/WHWgogogo/LyraNest-AirPlay-Bridge)：AirPlay 2 无线投送桥接插件。
- [LyraNest Xiaoai Bridge](https://github.com/WHWgogogo/LyraNest-Xiaoai-Bridge)：小爱音箱语音联动与投送桥接插件。
- [LyraNest Community](https://github.com/WHWgogogo/LyraNest-Community)：开源社区版。

---

## 开源许可与版权声明 (License)

本项目基于 **[CC BY-NC-SA 4.0 (知识共享 署名-非商业性使用-相同方式共享 4.0 国际许可协议)](LICENSE)** 开源。

### 商业限制特别说明：
1. **个人免费**：仅供个人学习、研究、家庭局域网与非营利性 NAS 环境免费使用。
2. **严禁商用**：未经作者书面明确许可，严禁任何个人或组织将本项目（包括源码、二进制文件、NAS 原生安装包、Docker 镜像）：
   - 用于任何直接或间接的商业营利行为；
   - 捆绑打包进任何收费硬件设备、品牌 NAS 主机、定制工控机或付费商业软件中；
   - 在电商平台（如淘宝、闲鱼、拼多多等）转售、倒卖安装包或提供付费安装/调试服务；
   - 作为商业公司闭源产品的组成部分。
3. **商业合作**：如有商业集成、预装或定制开发需求，请联系作者获取商业授权。

