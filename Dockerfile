# Base image olarak Go'nun resmi imajını kullan
FROM golang:1.24-alpine

# git lazım — `go mod download` bəzi paketlər üçün VCS metadata istəyir.
RUN apk add --no-cache git

# Proje dizinini ayarla
WORKDIR /app

# Bütün kaynak kodu kopyala — vendor/ də daxil olmaqla.
COPY . .

# Vendor uyğunsuzluğu olduqda (yeni go.mod paketi, lakin vendor/modules.txt
# yenilənməyib) build dayanır. Bu addım vendor-u yenidən qurur: tidy + vendor.
# Beləliklə go.mod-a paket əlavə etmək üçün yalnız go.mod düzəlişi kifayətdir —
# əl ilə `go mod vendor` çağırmağa ehtiyac yoxdur.
RUN go mod tidy && go mod vendor

# Uygulamayı derle
WORKDIR /app/cmd/main
RUN go build -o /app/main .

# Uygulamayı çalıştır
WORKDIR /app
CMD ["./main"]
