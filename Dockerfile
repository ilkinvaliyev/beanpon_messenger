# ── BUILD STAGE ─────────────────────────────────────────────────────────────
# Go binar-ı alpine-də qururuq (kiçik, sürətli).
FROM golang:1.24-alpine AS build

# git lazım — `go mod download` bəzi paketlər üçün VCS metadata istəyir.
RUN apk add --no-cache git

WORKDIR /app

# Bütün kaynak kodu kopyala — vendor/ də daxil olmaqla.
COPY . .

# Vendor uyğunsuzluğu olduqda (yeni go.mod paketi, lakin vendor/modules.txt
# yenilənməyib) build dayanır. Bu addım vendor-u yenidən qurur: tidy + vendor.
# Beləliklə go.mod-a paket əlavə etmək üçün yalnız go.mod düzəlişi kifayətdir.
RUN go mod tidy && go mod vendor

WORKDIR /app/cmd/main
RUN go build -o /app/main .

# ── RUNTIME STAGE ───────────────────────────────────────────────────────────
# Runtime-da ffmpeg + audiowaveform lazımdır (voice waveform üçün).
# ffmpeg bookworm apt repo-sundadır. audiowaveform Debian rəsmi repo-da YOXDUR
# → BBC-nin rəsmi bookworm .deb release-indən qurulur (asılılıqları `apt-get
# install ./file.deb` avtomatik həll edir).
FROM debian:bookworm-slim

ARG AUDIOWAVEFORM_VERSION=1.10.1
ARG AUDIOWAVEFORM_DEB=audiowaveform_${AUDIOWAVEFORM_VERSION}-1-12_amd64.deb

# ffmpeg MƏCBURİDİR (build fail etməlidir onsuz). audiowaveform BEST-EFFORT:
# .deb yüklənməsə/qurulmasa belə build DAYANMIR (`|| true`) — servis mütləq
# qalxsın deyə. audiowaveform yoxdursa waveform fallback-a düşür (mesaj yenə gedir).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        wget \
    && ( wget -q "https://github.com/bbc/audiowaveform/releases/download/${AUDIOWAVEFORM_VERSION}/${AUDIOWAVEFORM_DEB}" -O /tmp/audiowaveform.deb \
         && apt-get install -y --no-install-recommends /tmp/audiowaveform.deb \
         || echo "WARN: audiowaveform quraşdırılmadı — waveform fallback işlədiləcək" ) \
    && rm -f /tmp/audiowaveform.deb \
    && apt-get purge -y wget \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /app/main /app/main

# .env — köhnə tək-stage Dockerfile `COPY . .` ilə .env-i də image-ə salırdı,
# tətbiq isə godotenv.Load() ilə onu oxuyur (docker-compose env_file işlətmir).
# Multi-stage-də bunu açıq şəkildə kopyalamalıyıq, yoxsa DB dəyişənləri boş
# qalır və tətbiq "PostgreSQL environment variables are missing" ilə çökür.
COPY .env /app/.env

CMD ["./main"]
