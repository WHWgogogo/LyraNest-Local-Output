# ============================================================
# LyraNest 本机音频输出伴生容器
# ============================================================
# 自研 Go 外壳（services/local-output） + MPD 播放内核（独立进程，GPL-2.0）。
# MPD 不链接、不修改、不内嵌其源码，仅作为独立可执行文件随镜像分发。
#
# 关键点（来自 35/36 号实机报告）：
#   · 基础镜像固定 Alpine 3.20；该版本的 mpd 0.23.15 实测包含 alsa 输出插件，
#     并自带 flac/mp3/aac/ogg/opus/wavpack/DSD 解码器与 ffmpeg 兜底解码；
#   · alsa-utils 提供 amixer —— 没有它就无法执行 Auto-Mute 强制解静音；
#   · 容器内读不到 /proc/asound，所有自检必须走 /dev/snd + ALSA control API。

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/local-output ./cmd/local-output

FROM alpine:3.20
LABEL org.opencontainers.image.title="LyraNest Local Output" \
      org.opencontainers.image.description="Server-side 3.5mm/HDMI local audio output sidecar (self-developed shell + MPD playback kernel)" \
      org.opencontainers.image.version="0.1.0" \
      org.opencontainers.image.licenses="MIT AND GPL-2.0-only"

# mpd        : 播放内核（独立进程，GPL-2.0，不修改其代码）
# alsa-utils : amixer，用于 Auto-Mute 初始化与硬件音量
RUN apk add --no-cache mpd alsa-utils ca-certificates tzdata \
    && mkdir -p /var/lib/mpd/playlists /music

COPY --from=build /out/local-output /usr/local/bin/local-output
COPY assets/mpd.conf.template /etc/lyranest/mpd.conf.template

ENV LOCAL_OUTPUT_PORT=8091 \
    ALSA_CARD=0 \
    ALSA_PCM_DEVICE=0 \
    MPD_MIXER_CONTROL=Headphone \
    TZ=Asia/Shanghai

EXPOSE 8091

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=20s \
    CMD ["/usr/local/bin/local-output", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/local-output"]
