package models

import (
	"log"

	"gorm.io/gorm"
)

// IsMessagingBanned kullanıcının spam ban'ı olup olmadığını kontrol eder.
//
// Kural (basit ve net):
//
//	spam_bans tablosunda bu user_id'ye ait bir kayıt varsa VE
//	o kaydın deleted_at sütunu NULL ise → kullanıcı spam'dir, mesaj yazamaz.
//
// Önemli tasarım kararları:
//   - actions / expires_at sütunlarına BAKILMAZ. Yalnızca "kayıt var mı +
//     deleted_at null mu" yeterlidir. Böylece bu sütunlar veritabanında
//     mevcut olmasa bile (migration uyumsuzluğu) sorgu kırılmaz.
//   - SELECT yalnızca "id" sütununu çeker — SELECT * kullanılsaydı, gorm
//     struct'taki tüm alanları (actions vb.) sorgular ve eksik bir sütun
//     "column does not exist" SQL hatası verirdi. Bu hata önceki versiyonda
//     fonksiyonun sessizce false dönmesine ve spam kullanıcının geçmesine
//     yol açıyordu.
//   - deleted_at IS NULL filtresi açıkça yazılır (raw Count sorgusu).
//
// spam_bans tablosu beanpon (site) tarafından yönetilir; messenger yalnızca okur.
func IsMessagingBanned(db *gorm.DB, userID uint) bool {
	var count int64
	err := db.Table("spam_bans").
		Where("user_id = ?", userID).
		Where("deleted_at IS NULL").
		Count(&count).Error

	if err != nil {
		// Beklenmeyen DB hatası (bağlantı vb.) — logla. Kullanıcıyı
		// engellemiyoruz ki, geçici bir DB sorunu tüm yeni sohbetleri
		// kilitlemesin; ama hata görünür olsun diye loglanır.
		log.Printf("IsMessagingBanned: spam_bans sorgusu başarısız (user_id=%d): %v", userID, err)
		return false
	}

	return count > 0
}
