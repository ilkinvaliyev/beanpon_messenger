package models

import (
	"gorm.io/gorm"
	"time"
)

type ConversationParticipant struct {
	ID                 uint           `json:"id" gorm:"primaryKey"`
	ConversationID     uint           `json:"conversation_id" gorm:"not null;index"`
	UserID             uint           `json:"user_id" gorm:"not null;index"`
	Role               string         `json:"role" gorm:"type:varchar(10);default:'member';check:role IN ('owner','admin','member')"`
	Nickname           *string        `json:"nickname" gorm:"type:varchar(255)"`
	IsMuted            bool           `json:"is_muted" gorm:"default:false"`
	MutedUntil         *time.Time     `json:"muted_until"`
	IsRestricted       bool           `json:"is_restricted" gorm:"default:false"`
	ScreenshotDisabled bool           `json:"screenshot_disabled" gorm:"default:false"`
	LastReadAt         *time.Time     `json:"last_read_at"`
	LastReadMessageID  *string        `json:"last_read_message_id" gorm:"type:uuid"`
	MessageCount       int            `json:"message_count" gorm:"default:0"`
	JoinedAt           *time.Time     `json:"joined_at"`
	LeftAt             *time.Time     `json:"left_at"`
	KickedBy           *uint          `json:"kicked_by"`
	InviteTokenUsed    *string        `json:"invite_token_used" gorm:"type:varchar(32)"`
	DeletedAt          gorm.DeletedAt `json:"deleted_at" gorm:"index"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`

	// İlişkiler
	Conversation Conversation `json:"-" gorm:"foreignKey:ConversationID"`
	User         User         `json:"user" gorm:"foreignKey:UserID"`
}

type ConversationInvite struct {
	ID             uint       `json:"id" gorm:"primaryKey"`
	ConversationID uint       `json:"conversation_id" gorm:"not null;index"`
	InvitedBy      uint       `json:"invited_by" gorm:"not null"`
	InvitedUserID  uint       `json:"invited_user_id" gorm:"not null;index"`
	Status         string     `json:"status" gorm:"type:varchar(10);default:'pending';check:status IN ('pending','accepted','rejected','expired')"`
	ExpiresAt      *time.Time `json:"expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type MessageRead struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	MessageID      string    `json:"message_id" gorm:"type:uuid;not null;index"`
	UserID         uint      `json:"user_id" gorm:"not null;index"`
	ConversationID uint      `json:"conversation_id" gorm:"not null;index"`
	ReadAt         time.Time `json:"read_at"`
	CreatedAt      time.Time `json:"created_at"`

	// İlişkiler
	Message Message `json:"-" gorm:"foreignKey:MessageID"`
	User    User    `json:"user" gorm:"foreignKey:UserID"`
}

// --- Request/Response yapıları ---

type CreateGroupRequest struct {
	Name        string  `json:"name" binding:"required,min=1,max=255"`
	Description *string `json:"description"`
	MemberIDs   []uint  `json:"member_ids"` // opsiyonel, başlangıç üyeler
}

type GroupConversationResponse struct {
	ID            uint       `json:"id"`
	Name          string     `json:"name"`
	Avatar        *string    `json:"avatar"`
	Description   *string    `json:"description"`
	MemberCount   int        `json:"member_count"`
	MyRole        string     `json:"my_role"`
	IsMuted       bool       `json:"is_muted"`
	InviteToken   *string    `json:"invite_token,omitempty"` // sadece admin/owner görür
	LastMessageAt *time.Time `json:"last_message_at"`
	UnreadCount   int        `json:"unread_count"`
	CreatedAt     time.Time  `json:"created_at"`
}

type GroupMessageResponse struct {
	ID             string    `json:"id"`
	ConversationID uint      `json:"conversation_id"`
	SenderID       uint      `json:"sender_id"`
	SenderName     string    `json:"sender_name"`
	SenderAvatar   *string   `json:"sender_avatar"`
	Text           string    `json:"text"`
	ReadCount      int       `json:"read_count"`
	TotalMembers   int       `json:"total_members"`
	CreatedAt      time.Time `json:"created_at"`
}

type MessageReadDetail struct {
	UserID   uint      `json:"user_id"`
	Username string    `json:"username"`
	Avatar   *string   `json:"avatar"`
	ReadAt   time.Time `json:"read_at"`
}
