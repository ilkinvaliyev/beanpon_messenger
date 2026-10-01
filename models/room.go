package models

import (
	"time"

	"gorm.io/gorm"
)

// Room — açıq (public) söhbət otağı. Qrup çatının public variantı: istəyən
// yaradır, istəyən oxuyur; yazmaq üçün tək-klik join (mesaj göndərəndə avtomatik
// üzv). Owner + adminlər mesaj silir, otağı dondurur/silir. Mesajlar `messages`
// cədvəlini `room_id` ilə paylaşır. Siyahı karma `score`-a görə sıralanır.
//
// Qeyd: LiveRoom (LiveKit canlı yayım) BAŞQA konseptdir — bu tip `ChatRoom`
// adlanır ki, qarışmasın; cədvəl adı `rooms`.
type ChatRoom struct {
	ID          uint    `json:"id" gorm:"primaryKey"`
	OwnerID     uint    `json:"owner_id" gorm:"not null;index"`
	Name        string  `json:"name" gorm:"type:varchar(255);not null"`
	Description *string `json:"description" gorm:"type:text"`
	Avatar      *string `json:"avatar" gorm:"type:varchar(500)"`
	// Dondurulmuş otaq: heç kim görmür/yaza bilmir (yalnız admin idarə edir).
	IsFrozen bool `json:"is_frozen" gorm:"default:false;index"`
	// Yeni gələn üzv əvvəlki mesajları görsünmü? Yaradan seçir.
	HistoryVisible bool `json:"history_visible" gorm:"default:true"`
	// Karma sıralama sayğacları.
	JoinCount      uint           `json:"join_count" gorm:"default:0"`
	MessageCount   uint           `json:"message_count" gorm:"default:0"`
	Score          float64        `json:"score" gorm:"default:0;index"`
	LastActivityAt *time.Time     `json:"last_activity_at" gorm:"index"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at" gorm:"index"`

	// İlişkilər
	Owner User `json:"owner" gorm:"foreignKey:OwnerID"`
}

func (ChatRoom) TableName() string { return "rooms" }

// RoomMember — otaq üzvü. role: owner | admin | member. Member = join edən
// (tək-klik). Oxumaq üçün üzvlük tələb olunmur; yazmaq üçün tələb olunur.
type RoomMember struct {
	ID              uint       `json:"id" gorm:"primaryKey"`
	RoomID          uint       `json:"room_id" gorm:"not null;index"`
	UserID          uint       `json:"user_id" gorm:"not null;index"`
	Role            string     `json:"role" gorm:"type:varchar(10);default:'member'"`
	JoinedAt        *time.Time `json:"joined_at"`
	LastReadAt      *time.Time `json:"last_read_at"`
	LastReadMessage *string    `json:"last_read_message_id" gorm:"type:varchar(36)"`
	// Per-user ayarlar (group chat paritesi): otaq "Söhbətlər" siyahısında
	// görünəndə bunlar fərdi tənzimləmələrdir.
	IsMuted    bool       `json:"is_muted" gorm:"default:false"`
	MutedUntil *time.Time `json:"muted_until"`
	IsArchived bool       `json:"is_archived" gorm:"default:false"`
	ArchivedAt *time.Time `json:"archived_at"`
	IsPinned   bool       `json:"is_pinned" gorm:"default:false"`
	PinnedAt   *time.Time `json:"pinned_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	// İlişkilər
	User User `json:"user" gorm:"foreignKey:UserID"`
}

func (RoomMember) TableName() string { return "room_members" }

// IsOwner — bu üzv otağın sahibidir.
func (m RoomMember) IsOwner() bool { return m.Role == "owner" }

// IsAdmin — admin rolu (owner DEYİL, ayrıca admin).
func (m RoomMember) IsAdmin() bool { return m.Role == "admin" }

// HasAdminAccess — moderasiya hüququ (mesaj sil, freeze): owner VƏ YA admin.
func (m RoomMember) HasAdminAccess() bool { return m.Role == "owner" || m.Role == "admin" }

// --- Request / Response ---

type CreateRoomRequest struct {
	Name           string  `json:"name" binding:"required,min=1,max=255"`
	Description    *string `json:"description"`
	HistoryVisible *bool   `json:"history_visible"` // nil → default true
}

type RoomResponse struct {
	ID             uint       `json:"id"`
	Name           string     `json:"name"`
	Description    *string    `json:"description"`
	Avatar         *string    `json:"avatar"`
	OwnerID        uint       `json:"owner_id"`
	IsFrozen       bool       `json:"is_frozen"`
	HistoryVisible bool       `json:"history_visible"`
	JoinCount      uint       `json:"join_count"`
	MessageCount   uint       `json:"message_count"`
	MyRole         *string    `json:"my_role"` // nil = üzv deyil (yalnız oxuyur)
	IsMember       bool       `json:"is_member"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// RoomListItem — join olunmuş otağın "Söhbətlər" siyahısı üçün sətri (iOS
// GroupConversation paritesi). Fərdi unread/mute/archive/pin + son mesaj.
type RoomListItem struct {
	ID              uint       `json:"id"`
	Name            string     `json:"name"`
	Avatar          *string    `json:"avatar"`
	MyRole          string     `json:"my_role"`
	MemberCount     int        `json:"member_count"`
	IsFrozen        bool       `json:"is_frozen"`
	LastMessageText *string    `json:"last_message_text"`
	LastSender      *string    `json:"last_sender_username"`
	LastMessageAt   *time.Time `json:"last_message_at"`
	UnreadCount     int        `json:"unread_count"`
	IsMuted         bool       `json:"is_muted"`
	IsArchived      bool       `json:"is_archived"`
	IsPinned        bool       `json:"is_pinned"`
	PinnedAt        *time.Time `json:"pinned_at"`
}

type RoomMessageResponse struct {
	ID             string  `json:"id"`
	RoomID         uint    `json:"room_id"`
	SenderID       uint    `json:"sender_id"`
	SenderName     string  `json:"sender_name"`
	SenderUsername string  `json:"sender_username"`
	SenderAvatar   *string `json:"sender_avatar"`
	SenderVerified bool    `json:"sender_verified"`
	Text           string  `json:"text"`
	// Reply — reply edilən mesaj blok/silinmişsə ReplyBlocked=true və mətn boş
	// (client "erişiminiz yoxdur" kartı göstərir).
	ReplyToID    *string   `json:"reply_to_message_id"`
	ReplyText    *string   `json:"reply_text"`
	ReplySender  *string   `json:"reply_sender_username"`
	ReplyBlocked bool      `json:"reply_blocked"`
	CreatedAt    time.Time `json:"created_at"`
}
