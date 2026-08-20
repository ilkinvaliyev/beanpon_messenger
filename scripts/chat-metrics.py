#!/usr/bin/env python3
"""chat-metrics — mesajlaşma ölçümlerini okunur şekilde göster.

Grafana/Prometheus kurmadan, tek komutla:

    ./scripts/chat-metrics.py
    ./scripts/chat-metrics.py http://10.10.0.5:5082/metrics

İki ölçüm arasındaki FARKI göster (asıl işe yarayan mod — "şu an ne oluyor"):

    ./scripts/chat-metrics.py --watch 60

Dosyadan oku (hata ayıklama):

    ./scripts/chat-metrics.py /tmp/metrics.txt

NOT: Tek çekimdeki sayılar SUNUCU AÇILIŞINDAN BERİ TOPLAM'dır. Yani ortalama
tüm zamanın ortalamasıdır — dünkü yavaşlık bugünkü rakamı kirletir. Anlık
davranış için --watch kullanın.

Gereksinim: python3 (harici paket yok).
"""

import sys
import time
import urllib.request
from collections import defaultdict

P = "beanpon_messenger_"
DEFAULT_URL = "http://127.0.0.1:5082/metrics"


# ── Prometheus metin formatı ayrıştırıcı ───────────────────────────────────

def parse(text):
    """-> (histograms, gauges)

    histograms[(ad, etiketler)] = {"sum": float, "count": float,
                                   "buckets": [(le, cumulative), ...]}
    gauges[ad] = float
    """
    hist = defaultdict(lambda: {"sum": 0.0, "count": 0.0, "buckets": []})
    gauges = {}

    for line in text.splitlines():
        if not line or line[0] == "#" or not line.startswith(P):
            continue
        try:
            head, value = line.rsplit(" ", 1)
            value = float(value)
        except ValueError:
            continue

        if "{" in head:
            name, labels = head.split("{", 1)
            labels = labels.rstrip("}")
        else:
            name, labels = head, ""

        if name.endswith("_bucket"):
            base = name[: -len("_bucket")]
            le = None
            keep = []
            for part in split_labels(labels):
                k, v = part
                if k == "le":
                    le = float("inf") if v == "+Inf" else float(v)
                else:
                    keep.append(f'{k}="{v}"')
            hist[(base, ",".join(sorted(keep)))]["buckets"].append((le, value))
        elif name.endswith("_sum"):
            base = name[: -len("_sum")]
            hist[(base, norm(labels))]["sum"] = value
        elif name.endswith("_count"):
            base = name[: -len("_count")]
            hist[(base, norm(labels))]["count"] = value
        else:
            gauges[name] = value
    return hist, gauges


def split_labels(labels):
    """`a="1",b="2"` -> [("a","1"), ("b","2")] — virgül içeren değere dayanıklı."""
    out, key, buf, in_str = [], None, "", False
    i = 0
    while i < len(labels):
        ch = labels[i]
        if not in_str:
            if ch == "=":
                key, buf = buf.strip(), ""
            elif ch == '"':
                in_str = True
            elif ch == ",":
                buf = ""
            else:
                buf += ch
        else:
            if ch == "\\" and i + 1 < len(labels):
                buf += labels[i + 1]
                i += 2
                continue
            if ch == '"':
                in_str = False
                out.append((key, buf))
                buf = ""
            else:
                buf += ch
        i += 1
    return out


def norm(labels):
    return ",".join(sorted(f'{k}="{v}"' for k, v in split_labels(labels)))


# ── Yüzdelik (histogram kovalarından) ──────────────────────────────────────

def quantile(h, q):
    """Kova sınırından yaklaşık yüzdelik. `<= X ms` anlamında okunmalı."""
    if h["count"] <= 0 or not h["buckets"]:
        return None
    target = q * h["count"]
    for le, cum in sorted(h["buckets"]):
        if cum >= target:
            return le
    return None


def fmt(sec):
    if sec is None:
        return "-"
    if sec == float("inf"):
        return ">2 s"
    if sec < 0.001:
        return f"{sec * 1e6:.0f} µs"
    if sec < 1:
        return f"{sec * 1e3:.1f} ms"
    return f"{sec:.2f} s"


def pad(s, w):
    """Türkçe karakterler tek görünür genişliktedir; str uzunluğu doğrudur."""
    return s + " " * max(0, w - len(s))


def rpad(s, w):
    return " " * max(0, w - len(s)) + s


# ── Raporlama ──────────────────────────────────────────────────────────────

W_LABEL, W_NUM = 28, 11


def header(title):
    print(f"\n=== {title} ===")
    print("  " + pad("", W_LABEL) + rpad("adet", 9) + rpad("ortalama", W_NUM)
          + rpad("~p50", W_NUM) + rpad("~p95", W_NUM))


def row(hist, title, name, labels, note_if=None):
    h = hist.get((P + name, norm(labels)))
    if not h or h["count"] == 0:
        print("  " + pad(title, W_LABEL) + rpad("0", 9) + rpad("-", W_NUM)
              + rpad("-", W_NUM) + rpad("-", W_NUM))
        return 0
    avg = h["sum"] / h["count"]
    line = ("  " + pad(title, W_LABEL) + rpad(f"{h['count']:.0f}", 9)
            + rpad(fmt(avg), W_NUM) + rpad(fmt(quantile(h, 0.50)), W_NUM)
            + rpad(fmt(quantile(h, 0.95)), W_NUM))
    if note_if and note_if(h):
        line += "   <-- " + note_if(h)
    print(line)
    return h["count"]


def gauge_line(gauges, title, name, warn=None):
    v = gauges.get(P + name, 0)
    line = "  " + pad(title, W_LABEL + 9) + rpad(f"{v:,.0f}", 12)
    if warn:
        w = warn(v)
        if w:
            line += "   <-- " + w
    print(line)


def report(hist, gauges, window=None):
    if window:
        print(f"\n>>> SON {window:.0f} SANİYE <<<")
    else:
        print("\n>>> SUNUCU AÇILIŞINDAN BERİ TOPLAM <<<")
        print("    (anlık durum için: --watch 60)")

    header("MESAJ GÖNDERME (sunucu tarafındaki toplam süre)")
    row(hist, "WebSocket (yeni iOS)", "dm_send_duration_seconds",
        'transport="ws",result="ok"')
    row(hist, "REST (Flutter / eski iOS)", "dm_send_duration_seconds",
        'transport="rest",result="ok"')
    row(hist, "  reddedilen", "dm_send_duration_seconds",
        'transport="ws",result="rejected"')
    row(hist, "  hatalı", "dm_send_duration_seconds",
        'transport="ws",result="error"',
        note_if=lambda h: "HATA VAR" if h["count"] > 0 else None)
    row(hist, "  yinelenen", "dm_send_duration_seconds",
        'transport="ws",result="duplicate"')

    header("SÜRE NEREDE GEÇİYOR (WebSocket)")
    steps = {}
    for key, title in (("perm", "izin kontrolleri"),
                       ("persist", "veritabanına yazma"),
                       ("fanout", "yayın + push kapısı")):
        h = hist.get((P + "dm_send_step_seconds", norm(f'transport="ws",step="{key}"')))
        steps[title] = (h["sum"] / h["count"]) if h and h["count"] else 0
        row(hist, title, "dm_send_step_seconds", f'transport="ws",step="{key}"')
    if any(steps.values()):
        worst = max(steps, key=steps.get)
        print(f"\n  → En pahalı adım: {worst.upper()}  (bir sonraki işimiz burası)")

    header("AĞIR SORGULAR")
    for q, title in (("conversations", "sohbet listesi"),
                     ("history", "sohbet geçmişi"),
                     ("sync", "delta-sync (yeniden bağlanma)"),
                     ("unread", "okunmamış sayacı")):
        row(hist, title, "query_duration_seconds", f'query="{q}"')

    header("PUSH BİLDİRİMİ")
    row(hist, "gönderildi", "push_duration_seconds", 'result="sent"')
    row(hist, "başarısız", "push_duration_seconds", 'result="failed"',
        note_if=lambda h: "push gitmiyor" if h["count"] > 0 else None)

    print("\n=== BAĞLANTILAR ===")
    gauge_line(gauges, "bağlı istemci", "ws_clients")
    gauge_line(gauges, "en dolu gönderim kuyruğu (256)", "ws_send_queue_max",
               warn=lambda v: "DOLMAK ÜZERE" if v > 128 else None)
    gauge_line(gauges, "kopartılan bağlantı (toplam)", "ws_evicted_total",
               warn=lambda v: "yavaş istemci sorunu (W3)" if v > 0 else None)

    print("\n=== KÜME YAYINI ===")
    gauge_line(gauges, "yapılmayan yayın (tek instans)", "cluster_skipped_solo_total",
               warn=lambda v: "C3 kazancı" if v > 0 else None)
    gauge_line(gauges, "yapılan yayın", "cluster_published_total")
    gauge_line(gauges, "ATILAN frame (kuyruk dolu)", "cluster_dropped_total",
               warn=lambda v: "CANLI YAYIN KAYBI" if v > 0 else None)
    print()


# ── Fark alma (--watch) ────────────────────────────────────────────────────

def diff(a_hist, a_g, b_hist, b_g):
    """b - a. Histogramlarda sum/count/kovalar, sayaçlarda fark; gauge'lar son hâl."""
    out = defaultdict(lambda: {"sum": 0.0, "count": 0.0, "buckets": []})
    for key, b in b_hist.items():
        a = a_hist.get(key)
        if not a:
            out[key] = b
            continue
        ab = dict(a["buckets"])
        out[key] = {
            "sum": b["sum"] - a["sum"],
            "count": b["count"] - a["count"],
            "buckets": [(le, cum - ab.get(le, 0)) for le, cum in b["buckets"]],
        }
    g = {}
    for k, v in b_g.items():
        # `_total` ile bitenler sayaçtır → fark; diğerleri anlık değer.
        g[k] = v - a_g.get(k, 0) if k.endswith("_total") else v
    return out, g


def fetch(src):
    if src.startswith("http"):
        with urllib.request.urlopen(src, timeout=10) as r:
            return r.read().decode("utf-8", "replace")
    with open(src, encoding="utf-8") as f:
        return f.read()


def main():
    args = sys.argv[1:]
    window = None
    if "--watch" in args:
        i = args.index("--watch")
        window = float(args[i + 1]) if len(args) > i + 1 else 60.0
        del args[i:i + 2]
    src = args[0] if args else DEFAULT_URL

    try:
        text = fetch(src)
    except Exception as e:
        print(f"HATA: {src} okunamadı: {e}", file=sys.stderr)
        print("  • Sunucu ayakta mı?  docker ps | grep beanpon", file=sys.stderr)
        print("  • Adres:  ./scripts/chat-metrics.py http://10.10.0.5:5082/metrics",
              file=sys.stderr)
        sys.exit(1)

    if window is None:
        report(*parse(text))
        return

    a_hist, a_g = parse(text)
    print(f"İlk ölçüm alındı. {window:.0f} saniye bekleniyor…", file=sys.stderr)
    t0 = time.time()
    time.sleep(window)
    b_hist, b_g = parse(fetch(src))
    report(*diff(a_hist, a_g, b_hist, b_g), window=time.time() - t0)


if __name__ == "__main__":
    main()
