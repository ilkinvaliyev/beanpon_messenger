# Base image olarak Go'nun resmi imajını kullan
FROM golang:1.23-alpine

# Proje dizinini ayarla
WORKDIR /app

# Modül dosyasını ve bağımlılıkları kopyala
COPY go.mod ./

# Modülleri indir
RUN go mod download

# Uygulama dosyalarını kopyala
COPY . .

# Uygulamayı derle
WORKDIR /app/cmd/main
RUN go build -o /app/main .

# Uygulamayı çalıştır
WORKDIR /app
CMD ["./main"]
