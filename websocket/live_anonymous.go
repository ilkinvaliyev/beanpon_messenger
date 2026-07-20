package websocket

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"beanpon_messenger/database"
)

// Anonim live sohbeti (live_rooms.anonymous_audience).
//
// piokio_live/services/anonymous.go ve Laravel CommentResource::buildAnonAlias
// ile BIREBIR AYNI algoritma: sha256(room_id + "-" + user_id) -> ilk 8 hex ->
// mod 10000 -> "Anonym0042". Ayni kullanici ayni yayinda her yerde (katilimci
// listesi, sohbet, reply) AYNI takma adi tasir — servisler arasi tutarli.
//
// KIM GIZLENIR (piokio_live ile ayni kural):
//   · audience (dinleyici) + sohbet yazanlar -> gizli (Anonym####, sender_id=0)
//   · host, broadcaster                      -> ACIK (herkes bilir)
//
// DB'de sender_id HER ZAMAN gercek deger — anonimlestirme yalnizca yayin/
// response katmanindadir (admin kimin ne yazdigini gorur).

// BuildLiveAnonAlias — deterministic takma ad.
func BuildLiveAnonAlias(roomID, userID uint) string {
	h := sha256.Sum256([]byte(strconv.FormatUint(uint64(roomID), 10) + "-" + strconv.FormatUint(uint64(userID), 10)))
	prefix := hex.EncodeToString(h[:])[0:8]
	n, _ := strconv.ParseUint(prefix, 16, 64)
	return fmt.Sprintf("Anonym%04d", n%10000)
}

// --- Oda anonimlik durumu + sahne rolleri (kisa omurlu cache) ---
//
// Her mesajda DB'ye gitmemek icin oda basina 30 sn'lik cache. Live sohbet
// yuksek frekanslidir; bu cache olmadan her mesaj 2 sorgu ekstra maliyet olurdu.

type liveAnonState struct {
	anonymous bool
	// stageUsers — host + broadcaster id'leri (kimlikleri ACIK kalanlar).
	stageUsers map[uint]bool
	fetchedAt  time.Time
}

var (
	liveAnonMu    sync.RWMutex
	liveAnonCache = map[uint]*liveAnonState{}
)

const liveAnonTTL = 30 * time.Second

// liveAnonStateFor — odanin anonimlik durumunu ve sahnedeki kullanicilari
// getirir (cache'li). Hata durumunda anonim DEGIL kabul edilir (fail-open:
// ozellik calismazsa sohbet normal akar).
func liveAnonStateFor(roomID uint) *liveAnonState {
	liveAnonMu.RLock()
	st, ok := liveAnonCache[roomID]
	liveAnonMu.RUnlock()
	if ok && time.Since(st.fetchedAt) < liveAnonTTL {
		return st
	}

	fresh := &liveAnonState{stageUsers: map[uint]bool{}, fetchedAt: time.Now()}

	var row struct{ AnonymousAudience bool }
	if err := database.DB.Table("live_rooms").
		Select("anonymous_audience").
		Where("id = ?", roomID).
		Scan(&row).Error; err == nil {
		fresh.anonymous = row.AnonymousAudience
	}

	// Sahnedekiler yalnizca anonim yayinda gerekli.
	if fresh.anonymous {
		var ids []uint
		database.DB.Table("live_room_participants").
			Where("live_room_id = ? AND status = ? AND role IN ?",
				roomID, "active", []string{"host", "broadcaster"}).
			Pluck("user_id", &ids)
		for _, id := range ids {
			fresh.stageUsers[id] = true
		}
	}

	liveAnonMu.Lock()
	liveAnonCache[roomID] = fresh
	liveAnonMu.Unlock()
	return fresh
}

// InvalidateLiveAnonCache — rol degisiminde (sahneye cikma/inme) cagrilir ki
// yeni broadcaster'in kimligi hemen acilsin.
func InvalidateLiveAnonCache(roomID uint) {
	liveAnonMu.Lock()
	delete(liveAnonCache, roomID)
	liveAnonMu.Unlock()
}

// AnonymizeLiveSender — bir mesajin gonderen bilgisini (gerekiyorsa)
// anonimlestirir.
//
//	roomID, senderID — mesajin sahibi
//	viewerID         — mesaji ALAN kisi (0 = genel yayin, en katı davranis)
//
// Doner: (gosterilecek sender_id, gosterilecek isim, anonim mi).
// Kisi kendi mesajini her zaman gercek adiyla gorur.
func AnonymizeLiveSender(roomID, senderID, viewerID uint, realName string) (uint, string, bool) {
	st := liveAnonStateFor(roomID)
	if !st.anonymous {
		return senderID, realName, false
	}
	// Sahnedekiler (host/broadcaster) acik.
	if st.stageUsers[senderID] {
		return senderID, realName, false
	}
	// Kisi kendini gercek gorur.
	if viewerID != 0 && viewerID == senderID {
		return senderID, realName, false
	}
	return 0, BuildLiveAnonAlias(roomID, senderID), true
}
