# DEPLOY — çoxlu replica (horizontal scale) üçün WS klaster qatı (Issue 4)

## Nə düzəldildi

`Hub.clients` yalnız **bir prosesin yaddaşındakı** map idi və `SendToUser`
sırf ona baxırdı. `cache/redis.go`-da `Publish` / `Subscribe` **ümumiyyətlə
yox idi**. Tək replica ilə hər şey işləyir; **ikinci replica qalxdığı an**:


| Sındıran şey | Nəticə |
|---|---|
| A-dakı göndərən → B-dəki alıcı | Canlı mesaj **heç vaxt** çatmır (yalnız DB-də; alıcı çatı yenidən açanda görür) |
| `IsUserOnline` | B-dəki onlayn istifadəçi A-da "offline" görünür → çatda oturduğu halda push gedir |
| `IsUserInChatWith` / `IsUserInGroupChat` | Avto-oxundu və push susdurma qərarları səhv |
| yazır…, oxundu, reaksiya, qrup üzvlük event-ləri | Hamısı itir |

Yəni sistem **gizli şəkildə tək replica-ya pinlənmişdi**: ikinci replica əlavə
etmək heç bir xəta vermir, sadəcə mesajlaşmanı yarıdan bölür.

## Necə işləyir

**1. Yayım (fan-out).** Hər instans `bp:msg:ws:fanout` kanalına abunə olur.
`SendToUser` / `SendToMultipleUsers` əvvəlcə **öz lokal** client-lərinə çatdırır,
sonra kanala yayımlayır. Digər instanslar frame-i alıb yalnız **özündə olan**
alıcılara ötürür. Öz yayımını `origin` sahəsindən tanıyıb atır — ikiqat
çatdırma yoxdur.

**2. Paylaşılan presence.** Hər bağlı istifadəçi üçün `bp:msg:ws:presence:{id}`
yazılır: hansı instansda olduğu + hazırda açıq DM/qrup. TTL **90 s**, heartbeat
**30 s**-də bir yeniləyir. İnstans qəfil ölsə (SIGKILL/OOM/node itkisi) qeyd
öz-özünə yox olur — "zombi onlayn" qalmır.
Oxuma yolları əvvəlcə lokal map-ə baxır (sürətli yol dəyişməyib), tapmasa
Redis-ə düşür. Qrup üçün `FilterUsersInGroupChat` **tək MGET** işlədir
(5000 üzvlü qrupda üzv-başına GET fəlakət olardı).

## Konfiqurasiya

| Env | Default | Mənası |
|---|---|---|
| `REDIS_ENABLED` | mövcud dəyər | `false` olduqda bütün klaster qatı **no-op**-dur |
| `WS_CLUSTER_ENABLED` | `true` | `false` ilə açıq şəkildə söndürülür (Redis açıq olsa belə) |
| `INSTANCE_ID` | `<hostname>-<rand8>` | İnstans kimliyi. Kubernetes-də `metadata.name` verin |

**Tək replica ilə heç nə etməyə ehtiyac yoxdur** — qat işləyir, sadəcə hər
frame üçün bir əlavə Redis `PUBLISH` olur (öz yayımı gələndə atılır).
İstəsəniz `WS_CLUSTER_ENABLED=false` ilə tamamilə söndürə bilərsiniz.

## Redis tələbləri

* **Eyni Redis instansı** bütün replica-lar tərəfindən görünməlidir.
  `docker-compose.yml`-də `redis` servisi onsuz da paylaşılır.
* Pub/sub **master**-ə gedir (`REDIS_HOST`). `REDIS_READ_HOST` (replica) yalnız
  `GET`/`MGET` üçün işlənir — abunə oradan qurulmur.
* **Redis Cluster (sharded) işlədirsinizsə** diqqət: `PUBLISH` shard-lar arası
  yayılır, amma `SPUBLISH`/shard pub-sub istifadə edilmir. Tək master + replica
  (hazırkı quraşdırma) tam dəstəklənir.
* Yaddaş: presence açarı istifadəçi başına ~80 bayt. 100 000 eyni anda onlayn
  istifadəçi ≈ 8 MB.

## Load balancer

Sticky session **TƏLƏB OLUNMUR** — bu düzəlişin əsas məqsədi məhz odur.
Amma yenə də tövsiyə olunur (`ip_hash` və ya cookie affinity): eyni istifadəçi
eyni instansda qalırsa Redis üzərindən keçən trafik azalır.

WebSocket üçün nginx-də adi tələblər dəyişməyib:

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
proxy_read_timeout 3600s;
```

## Yoxlama

**1. Abunə quruldumu?** Hər replica-nın log-unda:

```
ws-cluster: instans kimliyi <id>
cache: "bp:msg:ws:fanout" kanalına abunə olundu
```

`ws-cluster: söndürülüb ...` görünürsə Redis bağlı deyil və ya
`WS_CLUSTER_ENABLED=false`.

**2. Canlı yayım işləyirmi?**

```bash
redis-cli SUBSCRIBE bp:msg:ws:fanout
# başqa terminalda bir mesaj göndər — frame görünməlidir:
# {"o":"<instance>","u":[7],"t":"new_message","d":{...}}
```

**3. Presence yazılırmı?**

```bash
redis-cli --scan --pattern 'bp:msg:ws:presence:*' | head
redis-cli GET bp:msg:ws:presence:7
redis-cli TTL bp:msg:ws:presence:7     # 60–90 arası olmalıdır
```

**4. Əsl ssenari testi (2 replica).**
İki fərqli replica-ya bağlanan iki cihazdan yazışın: mesaj **dərhal**
görünməlidir (səhifəni yeniləmədən). Əvvəl bu işləmirdi.

## Geri qaytarma

`WS_CLUSTER_ENABLED=false` → dərhal köhnə (tək instans) davranışa qayıdır.
Kod dəyişikliyini geri almağa ehtiyac yoxdur.

## Bilinən məhdudiyyətlər

* **Ortaq kanal** işlədilir (per-user kanal deyil): hər frame bütün
  instanslara gedir. 2–4 replica üçün əhəmiyyətsizdir. Replica sayı çox
  artarsa per-user kanala keçid asandır — funksiya imzaları dəyişmir.
* Yayım növbəsi sərhədlidir (4096 frame, 4 yazıcı). Dolarsa frame atılır və
  5 dəqiqədən bir loglanır (`ws-cluster: yayım növbəsi dolu — N frame atıldı`).
  Bu, **canlı** çatdırmadır: mesaj onsuz da DB-dədir və push gedir.
* Presence TTL 90 s-dir: instans qəfil ölsə istifadəçi ən çox 90 saniyə
  "onlayn" görünə bilər.
