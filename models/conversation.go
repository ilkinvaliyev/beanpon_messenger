package models

import (
	"gorm.io/gorm"
	"time"
)

type Conversation struct {
	ID                      uint       `json:"id" gorm:"primaryKey"`
	User1ID                 uint       `json:"user1_id" gorm:"not null;index"`
	User2ID                 uint       `json:"user2_id" gorm:"not null;index"`
	Status                  string     `json:"status" gorm:"type:varchar(20);default:'pending';check:status IN ('pending','active','restricted')"`
	Type                    string     `json:"type" gorm:"type:varchar(20);default:'request_based';check:type IN ('follow_based','request_based')"`
	User1MessageCount       int        `json:"user1_message_count" gorm:"default:0"`
	User2MessageCount       int        `json:"user2_message_count" gorm:"default:0"`
	MaxPendingMessages      int        `json:"max_pending_messages" gorm:"default:3"`
	User1FollowsUser2       bool       `json:"user1_follows_user2" gorm:"default:false"`
	User2FollowsUser1       bool       `json:"user2_follows_user1" gorm:"default:false"`
	MutualFollow            bool       `json:"mutual_follow" gorm:"default:false"`
	HasPreviousConversation bool       `json:"has_previous_conversation" gorm:"default:false"`
	FirstMessageAt          *time.Time `json:"first_message_at"`
	LastMessageAt           *time.Time `json:"last_message_at"`

	// ✅ YENİ: Son reaksiya bilgisi (conversations siyahısında göstərmək üçün)
	LastReactionEmoji    *string    `json:"last_reaction_emoji" gorm:"type:varchar(16)"`
	LastReactionAt       *time.Time `json:"last_reaction_at"`
	LastReactionByUserID *uint      `json:"last_reaction_by_user_id"`
	User1Muted           bool       `json:"user1_muted" gorm:"default:false"`
	User2Muted           bool       `json:"user2_muted" gorm:"default:false"`
	User1MutedAt         *time.Time `json:"user1_muted_at"`
	User2MutedAt         *time.Time `json:"user2_muted_at"`
	User1MutedUntil      *time.Time `json:"user1_muted_until"`
	User2MutedUntil      *time.Time `json:"user2_muted_until"`
	// Arxiv — per-user (mute pattern-i ilə eyni). A söhbəti arxivləyəndə
	// yalnız A-nın siyahısından gizlənir, B-də normal qalır. Üstəlik,
	// arxivləyən şəxsə (məs. A) gələn mesajlar üçün push notification
	// göndərilmir (Telegram-ın "arxiv = səssiz" davranışı).
	User1Archived   bool       `json:"user1_archived" gorm:"default:false"`
	User2Archived   bool       `json:"user2_archived" gorm:"default:false"`
	User1ArchivedAt *time.Time `json:"user1_archived_at"`
	User2ArchivedAt *time.Time `json:"user2_archived_at"`
	// Pin (sabitləmə) — per-user. A pin edəndə yalnız A-nın siyahısında ən
	// yuxarı gəlir, B-də normal sırada qalır. Bir neçə söhbət pin oluna bilər;
	// PinnedAt-a görə sıralanır (ən son pin → ən yuxarı).
	User1Pinned   bool       `json:"user1_pinned" gorm:"default:false"`
	User2Pinned   bool       `json:"user2_pinned" gorm:"default:false"`
	User1PinnedAt *time.Time `json:"user1_pinned_at"`
	User2PinnedAt *time.Time `json:"user2_pinned_at"`
	// Nickname (ləqəb) — per-user, birtərəfli. User1Nickname = user1-in qarşı
	// tərəf (user2) üçün qoyduğu ad (yalnız user1 görür). Boş → əsl ad.
	User1Nickname *string `json:"user1_nickname" gorm:"type:varchar(60)"`
	User2Nickname *string `json:"user2_nickname" gorm:"type:varchar(60)"`
	// Çat fonu (wallpaper) — per-user, konkret söhbət üçün. User1WallpaperID =
	// user1-in BU söhbət üçün seçdiyi wallpaper ID-si (Laravel chat_wallpapers).
	// NULL → qlobal seçim (user_settings), o da NULL-dursa default.
	User1WallpaperID   *uint      `json:"user1_wallpaper_id"`
	User2WallpaperID   *uint      `json:"user2_wallpaper_id"`
	User1Restricted    bool       `json:"user1_restricted" gorm:"default:false"`
	User2Restricted    bool       `json:"user2_restricted" gorm:"default:false"`
	RestrictionReason  *string    `json:"restriction_reason" gorm:"type:text"`
	TotalMessagesCount int        `json:"total_messages_count" gorm:"default:0"`
	StatusChangedAt    *time.Time `json:"status_changed_at"`
	FollowHistory      *string    `json:"follow_history" gorm:"type:jsonb"`

	// ✅ YENİ: Screenshot Disable
	User1ScreenshotDisabled   bool       `json:"user1_screenshot_disabled" gorm:"default:false"`
	User2ScreenshotDisabled   bool       `json:"user2_screenshot_disabled" gorm:"default:false"`
	User1ScreenshotDisabledAt *time.Time `json:"user1_screenshot_disabled_at"`
	User2ScreenshotDisabledAt *time.Time `json:"user2_screenshot_disabled_at"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"deleted_at" gorm:"index"`

	User1 User `json:"user1" gorm:"foreignKey:User1ID"`
	User2 User `json:"user2" gorm:"foreignKey:User2ID"`

	ChatType             string     `json:"chat_type" gorm:"type:varchar(10);default:'direct'"`
	GroupName            *string    `json:"group_name" gorm:"type:varchar(255)"`
	GroupAvatar          *string    `json:"group_avatar" gorm:"type:varchar(255)"`
	GroupDesc            *string    `json:"group_desc" gorm:"type:text"`
	CreatedBy            *uint      `json:"created_by"`
	InviteToken          *string    `json:"invite_token" gorm:"type:varchar(32);uniqueIndex"`
	InviteTokenExpiresAt *time.Time `json:"invite_token_expires_at"`
	MaxMembers           int        `json:"max_members" gorm:"default:150"`
	// Qrup admin icazələri (jsonb): {"allow_text":true,"allow_media":true,
	// "allow_gif":true,"allow_voice":true,"allow_circle_video":true}.
	// NULL = hamısı AÇIQ (default). Yalnız admin/owner dəyişə bilər;
	// bağlı əməliyyatı admin ÖZÜ edə bilər, digərləri 403 alır.
	GroupPermissions *string `json:"group_permissions" gorm:"type:jsonb"`
	// 📌 Sabitlənmiş (pin) mesaj — qrupda TƏK mesaj pin oluna bilər
	// (yenisi köhnəni əvəz edir). Yalnız admin/owner pin/unpin edir.
	// Pin olunmuş mesaj SİLİNƏNDƏ avtomatik NULL-lanır.
	PinnedMessageID *string `json:"pinned_message_id" gorm:"type:uuid"`
}

type ConversationResponse struct {
	ID                      uint       `json:"id"`
	OtherUserID             uint       `json:"other_user_id"`
	Status                  string     `json:"status"`
	Type                    string     `json:"type"`
	MyMessageCount          int        `json:"my_message_count"`
	OtherMessageCount       int        `json:"other_message_count"`
	IsMutedByMe             bool       `json:"is_muted_by_me"`
	IsRestrictedForMe       bool       `json:"is_restricted_for_me"`
	CanSendMessage          bool       `json:"can_send_message"`
	MaxPendingMessages      int        `json:"max_pending_messages"`
	HasPreviousConversation bool       `json:"has_previous_conversation"`
	LastMessageAt           *time.Time `json:"last_message_at"`

	// ✅ YENİ: Screenshot bilgisi
	IsScreenshotDisabled    bool `json:"is_screenshot_disabled"`    // Her iki taraftan biri disable ettiyse true
	MyScreenshotDisabled    bool `json:"my_screenshot_disabled"`    // Benim ayarım
	OtherScreenshotDisabled bool `json:"other_screenshot_disabled"` // Karşı tarafın ayarı

	CreatedAt time.Time `json:"created_at"`
}
