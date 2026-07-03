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
# Runtime-da ffmpeg + audiowaveform lazımdır (voice waveform üçün). Bunlar
# Debian-based image-də rahat qurulur; audiowaveform bookworm apt repo-sunda var.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ffmpeg \
        audiowaveform \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /app/main /app/main

CMD ["./main"]
