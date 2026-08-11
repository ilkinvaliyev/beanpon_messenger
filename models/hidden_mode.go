package models

import (
	"time"

	"gorm.io/gorm"
)

// Gizli Mod (Hidden Mode) — hesab səviyyəli məxfilik vəziyyəti (blok/deaktivdən
// AYRI). X gizli olduqda X yalnız öz close-friends'inə görünür; başqa hər kəs
// üçün deaktiv / mövcud olmayan hesab kimi davranır.
//
// Bu helper-lər digər Go servislərindəki (piokio_golang_main
// `CanViewHidden`) məntiqin — və Laravel `HiddenModeService::canView` —
// EYNİ SQL ilə köçürülməsidir. İD-lər bu paketdəki digər helper-lərlə
// (`IsBlocked`, `GetBlockedUserIDs`) uyğun olsun deyə `uint`-dir.
//
// Cədvəllər (beanpon ilə paylaşılan DB):
//   users.hidden_mode (bool) + users.hidden_until (nullable ts).
//     "gizli" = hidden_mode = true AND (hidden_until IS NULL OR hidden_until > NOW())
//   close_friend_lists(id, user_id=sahib) + close_friend_list_members(
//     close_friend_list_id, user_id=üzv). X-in close-friends'i = X-in
//     siyahılarının üzvləri.

// IsUserHidden — istifadəçi hazırda Gizli Mod'dadırmı (hidden_mode + müddət
// keçərli).
// GİZLİ MOD GEÇİCİ KAPALI (performans — DM açılış yavaşlığı). true yap → aç.
var hiddenModeEnabled = false

func IsUserHidden(db *gorm.DB, userID uint) bool {
	if !hiddenModeEnabled {
		return false
	}
	var row struct {
		HiddenMode  bool
		HiddenUntil *time.Time
	}
	if err := db.Table("users").
		Select("hidden_mode, hidden_until").
		Where("id = ?", userID).
		Take(&row).Error; err != nil {
		return false
	}
	if !row.HiddenMode {
		return false
	}
	return row.HiddenUntil == nil || row.HiddenUntil.After(time.Now())
}

// IsCloseFriendMember — viewerID, ownerID'nin hər hansı close-friends listesinin
// üyesi mi (owner tərəfindən əlavə edilibmi).
func IsCloseFriendMember(db *gorm.DB, ownerID, viewerID uint) bool {
	if viewerID == 0 {
		return false
	}
	var one int
	err := db.Raw(
		"SELECT 1 FROM close_friend_list_members cflm "+
			"JOIN close_friend_lists cfl ON cfl.id = cflm.close_friend_list_id "+
			"WHERE cfl.user_id = ? AND cflm.user_id = ? LIMIT 1",
		ownerID, viewerID,
	).Scan(&one).Error
	return err == nil && one == 1
}

// CanViewHidden — hedef Gizli Mod'daysa yalnız kendisi veya hedefin
// close-friends'i görebilir; aksi halde görünür (deaktiv gibi gizlenmez).
func CanViewHidden(db *gorm.DB, viewerID, targetID uint) bool {
	if viewerID == targetID {
		return true
	}
	if !IsUserHidden(db, targetID) {
		return true
	}
	return IsCloseFriendMember(db, targetID, viewerID)
}

// DMHiddenBlocked — 1:1 (direct) söhbət üçün Gizli Mod qapısı. viewer ↔ peer
// arasında mesajlaşma / görünürlük ENGELLENMELİYSE true qaytarır:
//   - peer gizlidir və viewer onun close-friend'i deyil (viewer onu görə bilmir), VƏ YA
//   - viewer özü gizlidir və peer viewer'in close-friend'i deyil (simmetrik / inbound).
//
// İkinci (simmetrik) yoxlama YALNIZ viewer gizli olduqda edilir — əlavə sorğu
// qənaəti. Yalnız DM üçündür; qrup söhbətlərinə tətbiq OLUNMAMALIDIR.
func DMHiddenBlocked(db *gorm.DB, viewerID, peerID uint) bool {
	if !hiddenModeEnabled {
		return false
	}
	if !CanViewHidden(db, viewerID, peerID) {
		return true
	}
	if IsUserHidden(db, viewerID) && !CanViewHidden(db, peerID, viewerID) {
		return true
	}
	return false
}

// HiddenBlockedPeerIDs — söhbət siyahısı (inbox) üçün BATCH Gizli Mod filtri.
// `GetBlockedUserIDs` deseni ilə eyni: hər sətir üçün ayrıca sorğu (N+1)
// əvəzinə ən çox 3 sorğu ilə, viewer'in GÖRMƏMƏLİ olduğu peer ID-lərinin
// map-ini qaytarır:
//   - peer gizlidir və viewer onun close-friend'i deyil, VƏ YA
//   - viewer özü gizlidir və peer viewer'in close-friend'i deyil.
//
// Boş nəticə = heç bir söhbət filtrlənmir. (DB uzaq host-da olduğu üçün siyahı
// yolunda per-row sorğudan qaçırıq — bax GetConversations N+1 qeydləri.)
func HiddenBlockedPeerIDs(db *gorm.DB, viewerID uint, peerIDs []uint) map[uint]bool {
	blocked := make(map[uint]bool)
	if len(peerIDs) == 0 {
		return blocked
	}

	// İstiqamət 1: gizli olan peer'lər.
	var hiddenPeers []uint
	db.Raw(`
		SELECT id FROM users
		WHERE id IN ?
		  AND hidden_mode = true
		  AND (hidden_until IS NULL OR hidden_until > NOW())
	`, peerIDs).Scan(&hiddenPeers)

	if len(hiddenPeers) > 0 {
		// Bu gizli peer'lərdən hansının close-friends listesinde viewer VAR
		// (yəni viewer onları hələ də görə bilir)?
		var visibleOwners []uint
		db.Raw(`
			SELECT cfl.user_id
			FROM close_friend_list_members cflm
			JOIN close_friend_lists cfl ON cfl.id = cflm.close_friend_list_id
			WHERE cfl.user_id IN ? AND cflm.user_id = ?
		`, hiddenPeers, viewerID).Scan(&visibleOwners)

		visible := make(map[uint]bool, len(visibleOwners))
		for _, id := range visibleOwners {
			visible[id] = true
		}
		for _, p := range hiddenPeers {
			if !visible[p] {
				blocked[p] = true // peer gizli, viewer onu görə bilmir
			}
		}
	}

	// İstiqamət 2: viewer özü gizlidirsə, yalnız öz close-friends'i ilə görünür.
	if IsUserHidden(db, viewerID) {
		var myCloseFriends []uint
		db.Raw(`
			SELECT cflm.user_id
			FROM close_friend_list_members cflm
			JOIN close_friend_lists cfl ON cfl.id = cflm.close_friend_list_id
			WHERE cfl.user_id = ? AND cflm.user_id IN ?
		`, viewerID, peerIDs).Scan(&myCloseFriends)

		friend := make(map[uint]bool, len(myCloseFriends))
		for _, id := range myCloseFriends {
			friend[id] = true
		}
		for _, p := range peerIDs {
			if !friend[p] {
				blocked[p] = true // viewer gizli, peer onun close-friend'i deyil
			}
		}
	}

	return blocked
}
